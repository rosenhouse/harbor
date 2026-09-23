package registry

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
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

type fakeNamespaces []string

func (f fakeNamespaces) Allows(ns string) bool { return slices.Contains(f, ns) }
func (f fakeNamespaces) Namespaces() []string  { return f }

// fixture serves project "proj" from a fake Harbor to namespaces ns1 and ns2.
type fixture struct {
	harbor       *fakeHarbor
	clock        *testingclock.FakeClock
	poller       *Poller
	repositories *Repositories
	artifacts    *Artifacts
}

const stalenessLimit = time.Minute

// newUnreadFixture returns a fixture that has not read Harbor yet.
func newUnreadFixture(h *fakeHarbor) *fixture {
	s := NewStore(stalenessLimit, fakeNamespaces{"ns1", "ns2"})
	c := testingclock.NewFakeClock(time.Now())
	s.clock = c
	return &fixture{harbor: h, clock: c, poller: NewPoller(h, "proj", s), repositories: NewRepositories(s), artifacts: NewArtifacts(s)}
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
