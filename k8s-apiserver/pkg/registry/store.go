package registry

import (
	"cmp"
	"slices"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/clock"
)

// Store holds a Harbor project as polled, and serves a copy of it to each namespace that sees the project.
type Store struct {
	project        string
	stalenessLimit time.Duration
	namespaces     Namespaces
	clock          clock.Clock

	mu sync.Mutex
	// items is replaced but never modified, so requests read it after releasing mu.
	items map[key]*item
	// listed is when the last poll that listed the repositories started, and err is why the polls since then failed.
	listed time.Time
	err    error
	// olderArtifacts maps each repository whose artifacts come from an earlier poll to when that poll started.
	olderArtifacts map[string]time.Time
}

type key struct{ resource, name string }

// item is an object without a namespace or UID.
type item struct {
	obj      object
	harborID int64
}

// object returns a copy of the item's object of resource in namespace.
func (it *item) object(resource, namespace string) object {
	o := it.obj.DeepCopyObject().(object)
	o.SetNamespace(namespace)
	o.SetUID(uid(namespace, resource, it.harborID))
	return o
}

// NewStore returns a store of project that fails requests once the data they need is older than stalenessLimit.
func NewStore(project string, stalenessLimit time.Duration, n Namespaces) *Store {
	return &Store{project: project, stalenessLimit: stalenessLimit, namespaces: n, clock: clock.RealClock{}}
}

// update replaces the items with those of the poll that started at listed.
func (s *Store) update(items map[key]*item, listed time.Time, olderArtifacts map[string]time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items, s.listed, s.err, s.olderArtifacts = items, listed, nil, olderArtifacts
}

// failed records why a poll failed.
func (s *Store) failed(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// snapshot returns the items, or an error once the repository list is older than the staleness limit.
// It also fails once the artifacts of a repository that the response needs are older than the limit.
func (s *Store) snapshot(needs func(repository string) bool) (map[key]*item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clock.Since(s.listed) > s.stalenessLimit {
		switch {
		case s.err != nil:
			return nil, apierrors.NewServiceUnavailable(errorKind(s.err).Error())
		case s.listed.IsZero():
			return nil, apierrors.NewServiceUnavailable("harbor has not been read yet")
		}
		return nil, apierrors.NewServiceUnavailable(errSlowRead.Error())
	}
	for repository, started := range s.olderArtifacts {
		if s.clock.Since(started) > s.stalenessLimit && needs(repository) {
			return nil, apierrors.NewServiceUnavailable(errArtifactsRead.Error())
		}
	}
	return s.items, nil
}

// covered returns, sorted, the namespaces that see the project among those that a request for namespace covers.
func (s *Store) covered(namespace string) []string {
	switch {
	case namespace == "":
		return s.namespaces.Namespaces()
	case s.namespaces.Allows(namespace):
		return []string{namespace}
	}
	return nil
}

func (s *Store) get(resource schema.GroupResource, namespace, name string, needs func(repository string) bool) (runtime.Object, error) {
	if !s.namespaces.Allows(namespace) {
		return nil, apierrors.NewNotFound(resource, name)
	}
	items, err := s.snapshot(needs)
	if err != nil {
		return nil, err
	}
	it := items[key{resource.Resource, name}]
	if it == nil {
		return nil, apierrors.NewNotFound(resource, name)
	}
	return it.object(resource.Resource, namespace), nil
}

// list returns the objects of resource that match, in the namespaces that a request for namespace covers, sorted by namespace and name.
func (s *Store) list(resource schema.GroupResource, namespace string, match func(object, string) bool, needs func(repository string) bool) ([]runtime.Object, error) {
	namespaces := s.covered(namespace)
	if len(namespaces) == 0 {
		return nil, nil
	}
	items, err := s.snapshot(needs)
	if err != nil {
		return nil, err
	}
	var keys []key
	for k := range items {
		if k.resource == resource.Resource {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b key) int { return cmp.Compare(a.name, b.name) })
	var objs []runtime.Object
	for _, ns := range namespaces {
		for _, k := range keys {
			if it := items[k]; match(it.obj, ns) {
				objs = append(objs, it.object(resource.Resource, ns))
			}
		}
	}
	return objs, nil
}
