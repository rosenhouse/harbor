package registry

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	testingclock "k8s.io/utils/clock/testing"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

// fakeHarbor serves repositories in project "proj" and records calls.
type fakeHarbor struct {
	repositories []harbor.Repository
	// artifacts maps full repository names to their artifacts. Other repositories are not found.
	artifacts map[string][]harbor.Artifact
	// artifactErrs maps full repository names to errors listing their artifacts.
	artifactErrs map[string]error
	delay        time.Duration
	// onList runs during each list of repositories.
	onList func()

	mu                    sync.Mutex
	err                   error
	calls                 []string
	inFlight, maxInFlight int
}

func (f *fakeHarbor) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	return f.err
}

func (f *fakeHarbor) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// repositoryLists returns how many times Harbor listed repositories.
func (f *fakeHarbor) repositoryLists() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == "list proj" {
			n++
		}
	}
	return n
}

func (f *fakeHarbor) ListRepositories(_ context.Context, project string) ([]harbor.Repository, error) {
	if err := f.record("list " + project); err != nil {
		return nil, err
	}
	if f.onList != nil {
		f.onList()
	}
	return f.repositories, nil
}

func (f *fakeHarbor) ListArtifacts(_ context.Context, project, repository string) ([]harbor.Artifact, error) {
	err := f.record("artifacts " + project + " " + repository)
	f.mu.Lock()
	f.inFlight++
	f.maxInFlight = max(f.maxInFlight, f.inFlight)
	f.mu.Unlock()
	time.Sleep(f.delay)
	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if err := f.artifactErrs[project+"/"+repository]; err != nil {
		return nil, err
	}
	artifacts, ok := f.artifacts[project+"/"+repository]
	if !ok {
		return nil, harbor.ErrNotFound
	}
	return artifacts, nil
}

// fixture serves project "proj" from a fake Harbor to namespaces ns1 and ns2.
type fixture struct {
	harbor       *fakeHarbor
	clock        *testingclock.FakeClock
	store        *Store
	poller       *Poller
	repositories *Repositories
	artifacts    *Artifacts
}

const stalenessLimit = time.Minute

// newUnreadFixture returns a fixture that has not read Harbor yet.
func newUnreadFixture(h *fakeHarbor) *fixture {
	s := NewStore(stalenessLimit)
	c := testingclock.NewFakeClock(time.Now())
	s.clock = c
	s.SetNamespace("ns1", true)
	s.SetNamespace("ns2", true)
	return &fixture{harbor: h, clock: c, store: s, poller: NewPoller(h, "proj", s), repositories: NewRepositories(s), artifacts: NewArtifacts(s)}
}

func newFixture(t *testing.T, h *fakeHarbor) *fixture {
	t.Helper()
	f := newUnreadFixture(h)
	f.poll(t)
	return f
}

func (f *fixture) poll(t *testing.T) {
	t.Helper()
	_ = f.poller.Poll(t.Context())
}

func inNamespace(ns string) context.Context {
	return genericapirequest.WithNamespace(context.Background(), ns)
}

// resourceVersion lists everything that storage serves and returns the list's resourceVersion.
func resourceVersion(t *testing.T, storage rest.Lister) string {
	t.Helper()
	list, err := storage.List(inNamespace(""), nil)
	if err != nil {
		t.Fatal(err)
	}
	return list.(metav1.ListInterface).GetResourceVersion()
}

func parseRV(t *testing.T, rv string) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(rv, 10, 64)
	if err != nil || n == 0 {
		t.Fatalf("resourceVersion %q: %v", rv, err)
	}
	return n
}

func startWatch(t *testing.T, storage rest.Watcher, namespace string, opts *metainternalversion.ListOptions) watch.Interface {
	t.Helper()
	w, err := storage.Watch(inNamespace(namespace), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
	return w
}

// nextEvent returns the next event.
func nextEvent(t *testing.T, w watch.Interface) watch.Event {
	t.Helper()
	select {
	case e, ok := <-w.ResultChan():
		if !ok {
			t.Fatal("watch closed")
		}
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return watch.Event{}
}

// nextEvents returns the next n events, described by describe.
func nextEvents(t *testing.T, w watch.Interface, n int) []string {
	t.Helper()
	var got []string
	for range n {
		select {
		case e, ok := <-w.ResultChan():
			if !ok {
				t.Fatalf("watch closed after %q", got)
			}
			got = append(got, describe(e))
		case <-time.After(5 * time.Second):
			t.Fatalf("got %q, then no event", got)
		}
	}
	return got
}

// expectEnd fails unless w closes without further events.
func expectEnd(t *testing.T, w watch.Interface) {
	t.Helper()
	select {
	case e, ok := <-w.ResultChan():
		if ok {
			t.Errorf("unexpected event %s", describe(e))
		}
	case <-time.After(5 * time.Second):
		t.Error("watch stayed open")
	}
}

// describe returns an event's type, and its object's namespace and name or its status reason.
func describe(e watch.Event) string {
	if status, ok := e.Object.(*metav1.Status); ok {
		return fmt.Sprintf("%s %s", e.Type, status.Reason)
	}
	o, err := meta.Accessor(e.Object)
	if err != nil {
		return fmt.Sprintf("%s %T", e.Type, e.Object)
	}
	return fmt.Sprintf("%s %s/%s", e.Type, o.GetNamespace(), o.GetName())
}

func expectNoEvent(t *testing.T, w watch.Interface) {
	t.Helper()
	select {
	case e := <-w.ResultChan():
		t.Errorf("unexpected event %s", describe(e))
	case <-time.After(50 * time.Millisecond):
	}
}
