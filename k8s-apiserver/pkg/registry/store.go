package registry

import (
	"cmp"
	"context"
	"fmt"
	"iter"
	"maps"
	"math/rand/v2"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	apistorage "k8s.io/apiserver/pkg/storage"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
)

// Store holds the last read of a Harbor project and serves a copy of it to each namespace that sees the project.
//
// Each change takes a range of resourceVersions, with one for each object that it changes in each namespace.
// A change in Harbor numbers the namespaces by their ordinals, and a namespace change numbers the objects by theirs,
// so an object's resourceVersion in a namespace follows from the two, without storing one for each pair.
//
// The log keeps the latest changes, so that a watch can resume from any resourceVersion in the log.
// Its size counts a namespace change once for each object. It keeps at least the latest batch of changes.
type Store struct {
	stalenessLimit   time.Duration
	logSize          int
	bookmarkInterval time.Duration
	clock            clock.WithTicker

	mu sync.Mutex
	// rv is the last resourceVersion that a change took.
	rv uint64
	// items is replaced but never modified, so requests read it after releasing mu. Only update replaces it.
	items map[key]*item
	// namespaces see the project, in ordinal order. Requests and the log hold earlier slices, so SetNamespace only appends to or copies it.
	namespaces                            []*labeledNamespace
	nextItemOrdinal, nextNamespaceOrdinal uint64
	log                                   []change
	logCost                               int
	// oldest is the oldest resourceVersion that a watch can resume from.
	oldest uint64
	// changed is closed when the log grows or the last read gets older.
	changed chan struct{}
	// read is when the oldest part of the last read of Harbor started, and readErr is why no later read replaced that part.
	read    time.Time
	readErr error
}

type key struct{ resource, name string }

// item is an object without a namespace, UID, or resourceVersion.
type item struct {
	obj      object
	harborID int64
	// rv is the first resourceVersion of the item's last change. ordinal stays the same while the object exists.
	rv, ordinal uint64
}

type labeledNamespace struct {
	name string
	// rv is the first resourceVersion of the change that made the namespace see the project.
	rv, ordinal uint64
}

// resourceVersion returns the resourceVersion of the object of it in ns.
func (ns *labeledNamespace) resourceVersion(it *item) uint64 {
	if it.rv > ns.rv {
		return it.rv + ns.ordinal
	}
	return ns.rv + it.ordinal
}

// event is a change to an object in a namespace. Its old or cur is nil when the object appears or disappears.
type event struct {
	rv        uint64
	namespace string
	key       key
	old, cur  *item
}

func (e event) object(it *item) object {
	o := it.obj.DeepCopyObject().(object)
	o.SetNamespace(e.namespace)
	o.SetUID(uid(e.namespace, e.key.resource, it.harborID))
	o.SetResourceVersion(strconv.FormatUint(e.rv, 10))
	return o
}

// watchEvent returns what a watch of the objects that match sees of e.
// An object that starts or stops matching appears or disappears.
func (e event) watchEvent(match func(object, string) bool) (watch.Event, bool) {
	oldMatches := e.old != nil && match(e.old.obj, e.namespace)
	curMatches := e.cur != nil && match(e.cur.obj, e.namespace)
	switch {
	case oldMatches && curMatches:
		return watch.Event{Type: watch.Modified, Object: e.object(e.cur)}, true
	case curMatches:
		return watch.Event{Type: watch.Added, Object: e.object(e.cur)}, true
	case oldMatches:
		return watch.Event{Type: watch.Deleted, Object: e.object(e.old)}, true
	}
	return watch.Event{}, false
}

// change is an entry in the log. It takes the resourceVersions from rv to last.
// A change in Harbor is an event without rv or namespace, which each of namespaces sees.
// A namespace change is an event for each object.
type change struct {
	rv, last   uint64
	harbor     *event
	namespaces []*labeledNamespace
	events     []event
}

// eventsFor returns the change's events for the objects of resource in namespace, or in all namespaces for "", in resourceVersion order.
func (c *change) eventsFor(resource, namespace string) iter.Seq[event] {
	return func(yield func(event) bool) {
		if c.harbor == nil {
			for _, e := range c.events {
				if e.key.resource == resource && (namespace == "" || e.namespace == namespace) && !yield(e) {
					return
				}
			}
			return
		}
		if c.harbor.key.resource != resource {
			return
		}
		for _, ns := range c.namespaces {
			e := *c.harbor
			e.rv, e.namespace = c.rv+ns.ordinal, ns.name
			if (namespace == "" || e.namespace == namespace) && !yield(e) {
				return
			}
		}
	}
}

func (c *change) cost() int {
	return max(1, len(c.events))
}

// NewStore returns a store that fails requests once its last read of Harbor is older than stalenessLimit.
func NewStore(stalenessLimit time.Duration) *Store {
	// A random start makes each replica reject, rather than misread, resourceVersions from other replicas.
	rv := rand.Uint64N(1<<52) + 1
	return &Store{
		stalenessLimit:   stalenessLimit,
		logSize:          10000,
		bookmarkInterval: time.Minute,
		clock:            clock.RealClock{},
		rv:               rv,
		oldest:           rv,
		items:            map[key]*item{},
		changed:          make(chan struct{}),
	}
}

// SetNamespace records whether a namespace sees the project.
func (s *Store) SetNamespace(name string, allowed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.namespaces, func(ns *labeledNamespace) bool { return ns.name == name })
	if (i >= 0) == allowed {
		return
	}
	c := change{rv: s.rv + 1}
	if allowed {
		ns := &labeledNamespace{name: name, rv: c.rv, ordinal: s.nextNamespaceOrdinal}
		s.nextNamespaceOrdinal++
		s.namespaces = append(s.namespaces, ns)
	} else {
		s.namespaces = slices.Delete(slices.Clone(s.namespaces), i, i+1)
	}
	for k, it := range s.items {
		e := event{rv: c.rv + it.ordinal, namespace: name, key: k, cur: it}
		if !allowed {
			e.old, e.cur = it, nil
		}
		c.events = append(c.events, e)
	}
	slices.SortFunc(c.events, func(a, b event) int { return cmp.Compare(a.rv, b.rv) })
	c.last = c.rv
	if n := len(c.events); n > 0 {
		c.last = c.events[n-1].rv
	}
	batch := len(s.log)
	s.appendLocked(c)
	s.publishLocked(batch)
}

// update replaces the last read of Harbor, recording a change for each object that differs.
// read is when the oldest part of items was read, and err is why later reads of that part failed.
// Calls must not overlap, since update reads s.items without holding mu.
func (s *Store) update(items map[key]*item, read time.Time, err error) {
	changes := diffItems(s.items, items)
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := len(s.log)
	for _, c := range changes {
		s.changeLocked(c.key, c.old, c.cur)
	}
	if len(changes) > 0 {
		s.items = items
	}
	older := read.Before(s.read)
	s.read, s.readErr = read, err
	switch {
	case len(changes) > 0:
		s.publishLocked(batch)
	case older:
		s.wakeLocked()
	}
}

// diffItems returns the changes from old to cur, in key order. It puts in cur each item of old that did not change.
func diffItems(old, cur map[key]*item) []event {
	var changes []event
	for _, k := range sortedKeys(old, cur) {
		o, c := old[k], cur[k]
		switch {
		case o == nil || c == nil:
			changes = append(changes, event{key: k, old: o, cur: c})
		case o.harborID != c.harborID:
			changes = append(changes, event{key: k, old: o}, event{key: k, cur: c})
		case equality.Semantic.DeepEqual(o.obj, c.obj):
			cur[k] = o
		default:
			changes = append(changes, event{key: k, old: o, cur: c})
		}
	}
	return changes
}

// failed records why a read of Harbor failed.
func (s *Store) failed(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readErr = err
}

// changeLocked logs a change to an object in Harbor.
func (s *Store) changeLocked(k key, old, cur *item) {
	c := change{rv: s.rv + 1, harbor: &event{key: k, old: old, cur: cur}, namespaces: s.namespaces}
	c.last = c.rv
	if n := len(s.namespaces); n > 0 {
		c.last += s.namespaces[n-1].ordinal
	}
	switch {
	case cur == nil:
	case old == nil:
		cur.rv, cur.ordinal = c.rv, s.nextItemOrdinal
		s.nextItemOrdinal++
	default:
		cur.rv, cur.ordinal = c.rv, old.ordinal
	}
	s.appendLocked(c)
}

func (s *Store) appendLocked(c change) {
	s.log = append(s.log, c)
	s.logCost += c.cost()
	s.rv = c.last
}

// publishLocked trims the log, keeping the changes from index batch on, and wakes watchers.
func (s *Store) publishLocked(batch int) {
	trim := 0
	for ; trim < batch && s.logCost > s.logSize; trim++ {
		s.logCost -= s.log[trim].cost()
		s.oldest = s.log[trim].last
	}
	if trim > 0 {
		s.log = slices.Clone(s.log[trim:])
	}
	s.wakeLocked()
}

func (s *Store) wakeLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func sortedKeys(itemSets ...map[key]*item) []key {
	var keys []key
	for _, items := range itemSets {
		keys = slices.AppendSeq(keys, maps.Keys(items))
	}
	slices.SortFunc(keys, func(a, b key) int {
		return cmp.Or(cmp.Compare(a.resource, b.resource), cmp.Compare(a.name, b.name))
	})
	return slices.Compact(keys)
}

// checkLocked returns an error if a request for namespace needs a fresher read of Harbor.
// A request that covers no namespace that sees the project needs none.
func (s *Store) checkLocked(namespace string) error {
	switch {
	case !s.coversLocked(namespace), s.clock.Now().Before(s.staleAtLocked()):
		return nil
	case s.readErr != nil:
		return apierrors.NewServiceUnavailable(errorKind(s.readErr).Error())
	case s.read.IsZero():
		return apierrors.NewServiceUnavailable("harbor has not been read yet")
	}
	return apierrors.NewServiceUnavailable(errSlowRead.Error())
}

// staleAtLocked returns the first time at which the last read is older than the staleness limit.
func (s *Store) staleAtLocked() time.Time {
	return s.read.Add(s.stalenessLimit + time.Nanosecond)
}

// coversLocked returns whether a request for namespace covers a namespace that sees the project.
func (s *Store) coversLocked(namespace string) bool {
	return slices.ContainsFunc(s.namespaces, func(ns *labeledNamespace) bool { return namespace == "" || ns.name == namespace })
}

// covered returns, sorted by name, the namespaces among nss that a request for namespace covers.
func covered(nss []*labeledNamespace, namespace string) []*labeledNamespace {
	var c []*labeledNamespace
	for _, ns := range nss {
		if namespace == "" || ns.name == namespace {
			c = append(c, ns)
		}
	}
	slices.SortFunc(c, func(a, b *labeledNamespace) int { return cmp.Compare(a.name, b.name) })
	return c
}

// stateLocked returns an event that adds each object of resource in the namespaces that a request for namespace covers.
// The events come from the current items and namespaces, which no one modifies, so they can be read after mu is released.
func (s *Store) stateLocked(resource, namespace string) iter.Seq[event] {
	items, namespaces := s.items, s.namespaces
	return func(yield func(event) bool) {
		var keys []key
		for k := range items {
			if k.resource == resource {
				keys = append(keys, k)
			}
		}
		slices.SortFunc(keys, func(a, b key) int { return cmp.Compare(a.name, b.name) })
		for _, ns := range covered(namespaces, namespace) {
			for _, k := range keys {
				it := items[k]
				if !yield(event{rv: ns.resourceVersion(it), namespace: ns.name, key: k, cur: it}) {
					return
				}
			}
		}
	}
}

// parseResourceVersion returns the resourceVersion that a request asks for, or 0 for any.
func parseResourceVersion(rv string) (uint64, error) {
	if rv == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(rv, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != rv {
		return 0, apierrors.NewBadRequest(fmt.Sprintf("invalid resource version %q", rv))
	}
	return n, nil
}

// checkStateLocked returns an error unless the current state satisfies a request for resourceVersion rv.
func (s *Store) checkStateLocked(rv uint64, match metav1.ResourceVersionMatch) error {
	switch {
	case rv > s.rv:
		return apistorage.NewTooLargeResourceVersionError(rv, s.rv, 1)
	case match == metav1.ResourceVersionMatchExact && rv < s.rv:
		return apierrors.NewResourceExpired(fmt.Sprintf("too old resource version: %d (%d)", rv, s.rv))
	}
	return nil
}

// checkResumeLocked returns an error unless a watch can resume from resourceVersion rv.
func (s *Store) checkResumeLocked(rv uint64) error {
	switch {
	case rv < s.oldest:
		return apierrors.NewResourceExpired(fmt.Sprintf("too old resource version: %d (%d)", rv, s.oldest))
	case rv > s.rv:
		// A later resourceVersion comes from another replica or process, so waiting for it would only delay a relist.
		return apierrors.NewResourceExpired(fmt.Sprintf("unknown resource version: %d (%d)", rv, s.rv))
	}
	return nil
}

func (s *Store) get(resource schema.GroupResource, namespace, name string, opts *metav1.GetOptions) (runtime.Object, error) {
	rv, err := parseResourceVersion(ptr.Deref(opts, metav1.GetOptions{}).ResourceVersion)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := cmp.Or(s.checkLocked(namespace), s.checkStateLocked(rv, "")); err != nil {
		return nil, err
	}
	k := key{resource.Resource, name}
	i := slices.IndexFunc(s.namespaces, func(ns *labeledNamespace) bool { return ns.name == namespace })
	it := s.items[k]
	if i < 0 || it == nil {
		return nil, apierrors.NewNotFound(resource, name)
	}
	return event{rv: s.namespaces[i].resourceVersion(it), namespace: namespace, key: k}.object(it), nil
}

func (s *Store) list(resource, namespace string, opts *metainternalversion.ListOptions, match func(object, string) bool) ([]runtime.Object, string, error) {
	if opts == nil {
		opts = &metainternalversion.ListOptions{}
	}
	rv, err := parseResourceVersion(opts.ResourceVersion)
	if err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	if err := cmp.Or(s.checkLocked(namespace), s.checkStateLocked(rv, opts.ResourceVersionMatch)); err != nil {
		s.mu.Unlock()
		return nil, "", err
	}
	state, current := s.stateLocked(resource, namespace), s.rv
	s.mu.Unlock()
	var objs []runtime.Object
	for e := range state {
		if we, ok := e.watchEvent(match); ok {
			objs = append(objs, we.Object)
		}
	}
	return objs, strconv.FormatUint(current, 10), nil
}

// changesAfter returns the logged changes after resourceVersion rv, a channel that is closed when there may be more,
// and when a request for namespace will need a fresher read of Harbor, or zero for never.
// It fails once a request for namespace needs a fresher read of Harbor.
func (s *Store) changesAfter(rv uint64, namespace string) ([]change, <-chan struct{}, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := cmp.Or(s.checkLocked(namespace), s.checkResumeLocked(rv)); err != nil {
		return nil, nil, time.Time{}, err
	}
	var staleAt time.Time
	if s.coversLocked(namespace) {
		staleAt = s.staleAtLocked()
	}
	i := sort.Search(len(s.log), func(i int) bool { return s.log[i].last > rv })
	return s.log[i:], s.changed, staleAt, nil
}

func (s *Store) watch(ctx context.Context, resource, namespace string, match func(object, string) bool, opts *metainternalversion.ListOptions, newObject func() runtime.Object) (watch.Interface, error) {
	rv, err := parseResourceVersion(opts.ResourceVersion)
	if err != nil {
		return nil, err
	}
	initial := ptr.Deref(opts.SendInitialEvents, rv == 0)
	w := newWatcher(s, resource, namespace, match, newObject)
	w.bookmarks = opts.AllowWatchBookmarks
	w.markInitialEnd = ptr.Deref(opts.SendInitialEvents, false) && opts.AllowWatchBookmarks

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(namespace); err != nil {
		return nil, err
	}
	switch {
	case initial:
		if err := s.checkStateLocked(rv, opts.ResourceVersionMatch); err != nil {
			return nil, err
		}
		w.initial = s.stateLocked(resource, namespace)
		w.rv = s.rv
	case rv == 0:
		w.rv = s.rv
	default:
		if err := s.checkResumeLocked(rv); err != nil {
			return nil, err
		}
		w.rv = rv
	}
	go w.run(ctx)
	return w, nil
}
