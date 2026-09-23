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

// Store holds the last read of a Harbor project and serves a copy of it to each namespace that sees the project.
type Store struct {
	stalenessLimit time.Duration
	namespaces     Namespaces
	clock          clock.Clock

	mu sync.Mutex
	// items is replaced but never modified, so requests read it after releasing mu.
	items map[key]*item
	// read is when the oldest part of the last read of Harbor started, and readErr is why no later read replaced that part.
	read    time.Time
	readErr error
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

// NewStore returns a store that fails requests once its last read of Harbor is older than stalenessLimit.
func NewStore(stalenessLimit time.Duration, n Namespaces) *Store {
	return &Store{stalenessLimit: stalenessLimit, namespaces: n, clock: clock.RealClock{}}
}

// update replaces the last read of Harbor.
// read is when the oldest part of items was read, and err is why later reads of that part failed.
func (s *Store) update(items map[key]*item, read time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items, s.read, s.readErr = items, read, err
}

// failed records why a read of Harbor failed.
func (s *Store) failed(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readErr = err
}

// snapshot returns the last read of Harbor, or an error once it is older than the staleness limit.
func (s *Store) snapshot() (map[key]*item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.clock.Since(s.read) <= s.stalenessLimit:
		return s.items, nil
	case s.readErr != nil:
		return nil, apierrors.NewServiceUnavailable(errorKind(s.readErr).Error())
	case s.read.IsZero():
		return nil, apierrors.NewServiceUnavailable("harbor has not been read yet")
	}
	return nil, apierrors.NewServiceUnavailable(errSlowRead.Error())
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

func (s *Store) get(resource schema.GroupResource, namespace, name string) (runtime.Object, error) {
	if !s.namespaces.Allows(namespace) {
		return nil, apierrors.NewNotFound(resource, name)
	}
	items, err := s.snapshot()
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
func (s *Store) list(resource, namespace string, match func(object, string) bool) ([]runtime.Object, error) {
	namespaces := s.covered(namespace)
	if len(namespaces) == 0 {
		return nil, nil
	}
	items, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	var keys []key
	for k := range items {
		if k.resource == resource {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b key) int { return cmp.Compare(a.name, b.name) })
	var objs []runtime.Object
	for _, ns := range namespaces {
		for _, k := range keys {
			if it := items[k]; match(it.obj, ns) {
				objs = append(objs, it.object(resource, ns))
			}
		}
	}
	return objs, nil
}
