package registry

import (
	"context"
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

	initial        []event
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

	for _, e := range w.initial {
		if !w.sendEvent(ctx, e) {
			return
		}
	}
	w.initial = nil
	if w.markInitialEnd && !w.sendBookmark(ctx, map[string]string{metav1.InitialEventsAnnotationKey: "true"}) {
		return
	}

	var tick <-chan time.Time
	if w.bookmarks {
		ticker := w.store.clock.NewTicker(w.store.bookmarkInterval)
		defer ticker.Stop()
		tick = ticker.C()
	}
	for {
		changes, changed, err := w.store.changesAfter(w.rv, w.namespace)
		if err != nil {
			status := err.(apierrors.APIStatus).Status()
			w.send(ctx, watch.Event{Type: watch.Error, Object: &status})
			return
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
		select {
		case <-changed:
		case <-tick:
			if !w.sendBookmark(ctx, nil) {
				return
			}
		case <-ctx.Done():
			return
		case <-w.stop:
			return
		}
	}
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
