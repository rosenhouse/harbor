package registry

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
)

// Store holds a Harbor project as polled, and serves a copy of it to each namespace that sees the project.
type Store struct {
	project        string
	stalenessLimit time.Duration
	namespaces     Namespaces
	clock          clock.Clock

	mu sync.Mutex
	// items and links are replaced but never modified, so requests read them after releasing mu.
	items map[key]*item
	links []replicationLink
	// listed is when the last poll that listed the repositories started, and err is why the polls since then failed.
	listed time.Time
	err    error
	// linksListed is when the last poll that listed the replication policies started, and linksErr is why the lists since then failed.
	linksListed time.Time
	linksErr    error
	// olderArtifacts maps each repository whose artifacts come from an earlier poll to when that poll started.
	olderArtifacts map[string]time.Time
}

type key struct{ resource, name string }

// item is an object without a namespace or UID.
type item struct {
	obj      object
	harborID int64
	// repository is an artifact's repository within the project. It is empty for other objects.
	repository string
}

// object returns a copy of the item's object of resource in namespace, linked to the replication of link if it is not nil.
func (it *item) object(resource, namespace string, link *replicationLink) object {
	o := it.obj.DeepCopyObject().(object)
	o.SetNamespace(namespace)
	o.SetUID(uid(namespace, resource, it.harborID))
	if link != nil {
		o.SetLabels(link.labels(o.GetLabels()))
		o.SetOwnerReferences([]metav1.OwnerReference{link.ownerReference()})
	}
	return o
}

// labels returns the labels of the item's object, with that of link if it is not nil.
func (it *item) labels(link *replicationLink) labels.Set {
	if link == nil {
		return it.obj.GetLabels()
	}
	return link.labels(it.obj.GetLabels())
}

// replicationLink links the artifacts that a replication copied to the replication, in the replication's namespace.
type replicationLink struct {
	// namespaceUID identifies the namespace that the replication was created in.
	namespaceUID types.UID
	name         string
	uid          types.UID
	// repositoryPrefix starts the name, within the project, of each repository that the replication copies into.
	repositoryPrefix string
}

func (l *replicationLink) labels(base map[string]string) labels.Set {
	return labels.Merge(base, labels.Set{v1alpha1.ReplicationLabel: l.name})
}

func (l *replicationLink) ownerReference() metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: replicationKind.Kind, Name: l.name, UID: l.uid}
}

// linkOf returns the link, among links, of the replication that copied the artifacts of repository, or nil.
func linkOf(links []replicationLink, repository string) *replicationLink {
	for i := range links {
		if strings.HasPrefix(repository, links[i].repositoryPrefix) {
			return &links[i]
		}
	}
	return nil
}

// NewStore returns a store of project that fails requests once the data they need is older than stalenessLimit.
func NewStore(project string, stalenessLimit time.Duration, n Namespaces) *Store {
	return &Store{project: project, stalenessLimit: stalenessLimit, namespaces: n, clock: clock.RealClock{}}
}

// update replaces the items with those of the poll that started at listed.
// It replaces the links too, unless listing the replication policies failed with linksErr.
func (s *Store) update(items map[key]*item, listed time.Time, olderArtifacts map[string]time.Time, links []replicationLink, linksErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items, s.listed, s.err, s.olderArtifacts = items, listed, nil, olderArtifacts
	s.linksErr = linksErr
	if linksErr == nil {
		s.links, s.linksListed = links, listed
	}
}

// failed records why a poll failed.
func (s *Store) failed(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// snapshot returns the items and links, or an error once the repository list is older than the staleness limit.
// It also fails once the artifacts of a repository that the response needs are older than the limit,
// or once the links are older than the limit and the response needs them.
// needs gets the link of the replication that copied the repository, if any.
func (s *Store) snapshot(needs func(repository string, link *replicationLink) bool, needsLinks bool) (map[key]*item, []replicationLink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clock.Since(s.listed) > s.stalenessLimit {
		switch {
		case s.err != nil:
			return nil, nil, apierrors.NewServiceUnavailable(errorKind(s.err).Error())
		case s.listed.IsZero():
			return nil, nil, apierrors.NewServiceUnavailable("harbor has not been read yet")
		}
		return nil, nil, apierrors.NewServiceUnavailable(errSlowRead.Error())
	}
	if needsLinks && s.clock.Since(s.linksListed) > s.stalenessLimit {
		return nil, nil, apierrors.NewServiceUnavailable(fmt.Sprintf("%v: %v", errLinksRead, errorKind(s.linksErr)))
	}
	for repository, started := range s.olderArtifacts {
		if s.clock.Since(started) > s.stalenessLimit && needs(repository, linkOf(s.links, repository)) {
			return nil, nil, apierrors.NewServiceUnavailable(errArtifactsRead.Error())
		}
	}
	return s.items, s.links, nil
}

// linksIn returns the links, among links, of the replications that were created in namespace.
// A namespace that is deleted and created again has a new UID, so it gets none of the old links.
func (s *Store) linksIn(links []replicationLink, namespace string) []replicationLink {
	ns, ok := s.namespaces.Namespace(namespace)
	if !ok {
		return nil
	}
	var in []replicationLink
	for _, l := range links {
		if l.namespaceUID == ns.UID {
			in = append(in, l)
		}
	}
	return in
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

func (s *Store) get(resource schema.GroupResource, namespace, name string, needs func(repository string, link *replicationLink) bool) (runtime.Object, error) {
	if !s.namespaces.Allows(namespace) {
		return nil, apierrors.NewNotFound(resource, name)
	}
	items, links, err := s.snapshot(needs, false)
	if err != nil {
		return nil, err
	}
	it := items[key{resource.Resource, name}]
	if it == nil {
		return nil, apierrors.NewNotFound(resource, name)
	}
	return it.object(resource.Resource, namespace, linkOf(s.linksIn(links, namespace), it.repository)), nil
}

// list returns the objects of resource that match, in the namespaces that a request for namespace covers, sorted by namespace and name.
// match gets each object's labels as they are in the namespace. needsLinks is whether the response depends on the links.
func (s *Store) list(resource schema.GroupResource, namespace string, match func(object, labels.Set, string) bool, needs func(repository string, link *replicationLink) bool, needsLinks bool) ([]runtime.Object, error) {
	namespaces := s.covered(namespace)
	if len(namespaces) == 0 {
		return nil, nil
	}
	items, links, err := s.snapshot(needs, needsLinks)
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
		nsLinks := s.linksIn(links, ns)
		for _, k := range keys {
			it := items[k]
			if link := linkOf(nsLinks, it.repository); match(it.obj, it.labels(link), ns) {
				objs = append(objs, it.object(resource.Resource, ns, link))
			}
		}
	}
	return objs, nil
}
