package registry

import (
	"context"
	"iter"
	"strconv"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
)

// watcher follows a Store's log for the objects of one resource that match.
type watcher struct {
	store     *Store
	resource  string
	namespace string
	match     func(object, string) bool
	newObject func() runtime.Object

	initial        iter.Seq[event]
	markInitialEnd bool
	bookmarks      bool
	// rv is the resourceVersion that the watcher has followed the log to.
	rv uint64

	result   chan watch.Event
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

func newWatcher(s *Store, resource, namespace string, match func(object, string) bool, newObject func() runtime.Object) *watcher {
	return &watcher{
		store:     s,
		resource:  resource,
		namespace: namespace,
		match:     match,
		newObject: newObject,
		result:    make(chan watch.Event),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

func (w *watcher) ResultChan() <-chan watch.Event { return w.result }

// Stop ends the watch and waits for it to end.
func (w *watcher) Stop() {
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.done
}

func (w *watcher) run(ctx context.Context) {
	defer close(w.done)
	defer close(w.result)

	if w.initial != nil {
		for e := range w.initial {
			if !w.sendEvent(ctx, e) {
				return
			}
		}
		w.initial = nil
	}

	var tick <-chan time.Time
	if w.bookmarks {
		ticker := w.store.clock.NewTicker(w.store.bookmarkInterval)
		defer ticker.Stop()
		tick = ticker.C()
	}
	bookmarkDue := false
	for {
		changes, changed, staleAt, err := w.store.changesAfter(w.rv, w.namespace)
		if err != nil {
			status := err.(apierrors.APIStatus).Status()
			w.send(ctx, watch.Event{Type: watch.Error, Object: &status})
			return
		}
		if w.markInitialEnd {
			if !w.sendBookmark(ctx, map[string]string{metav1.InitialEventsAnnotationKey: "true"}) {
				return
			}
			w.markInitialEnd = false
		}
		for _, c := range changes {
			for e := range c.eventsFor(w.resource, w.namespace) {
				if e.rv > w.rv && !w.sendEvent(ctx, e) {
					return
				}
			}
			w.rv = c.last
		}
		if len(changes) > 0 {
			continue
		}
		if bookmarkDue {
			if !w.sendBookmark(ctx, nil) {
				return
			}
			bookmarkDue = false
			continue
		}
		var ok bool
		if bookmarkDue, ok = w.wait(ctx, changed, staleAt, tick); !ok {
			return
		}
	}
}

// wait returns once the log may have grown, the store goes stale at staleAt, or a bookmark is due, which it reports.
// It returns false for ok once the watch ends.
func (w *watcher) wait(ctx context.Context, changed <-chan struct{}, staleAt time.Time, tick <-chan time.Time) (bookmarkDue, ok bool) {
	var stale <-chan time.Time
	if !staleAt.IsZero() {
		timer := w.store.clock.NewTimer(staleAt.Sub(w.store.clock.Now()))
		defer timer.Stop()
		stale = timer.C()
	}
	select {
	case <-changed:
	case <-stale:
	case <-tick:
		return true, true
	case <-ctx.Done():
		return false, false
	case <-w.stop:
		return false, false
	}
	return false, true
}

func (w *watcher) sendEvent(ctx context.Context, e event) bool {
	we, ok := e.watchEvent(w.match)
	return !ok || w.send(ctx, we)
}

func (w *watcher) send(ctx context.Context, e watch.Event) bool {
	select {
	case w.result <- e:
		return true
	case <-ctx.Done():
	case <-w.stop:
	}
	return false
}

// sendBookmark tells the client that it has seen every change up to w.rv.
func (w *watcher) sendBookmark(ctx context.Context, annotations map[string]string) bool {
	o := w.newObject().(object)
	o.SetResourceVersion(strconv.FormatUint(w.rv, 10))
	o.SetAnnotations(annotations)
	return w.send(ctx, watch.Event{Type: watch.Bookmark, Object: o})
}
