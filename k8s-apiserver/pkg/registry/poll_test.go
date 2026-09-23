package registry

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

func TestPollSkipsDeletedRepository(t *testing.T) {
	h := artifactHarbor()
	h.repositories = append(h.repositories, harbor.Repository{ID: 4, Name: "proj/deleted"})
	f := newFixture(t, h)
	if n := artifactNames(artifactItems(t, f.artifacts, "ns1", nil)); len(n) != 4 {
		t.Errorf("got %v", n)
	}
}

func TestPollListsArtifactsConcurrently(t *testing.T) {
	h := &fakeHarbor{artifacts: map[string][]harbor.Artifact{}, delay: 20 * time.Millisecond}
	var want []string
	for i := range 10 {
		repository := fmt.Sprintf("r%d", i)
		h.repositories = append(h.repositories, harbor.Repository{ID: int64(i), Name: "proj/" + repository})
		h.artifacts["proj/"+repository] = []harbor.Artifact{{ID: int64(i), Digest: digest("aaaaaaaaaaaa")}}
		want = append(want, "ns1/"+repository+".sha256-aaaaaaaaaaaa")
	}
	items := artifactItems(t, artifactsOf(t, h), "ns1", nil)
	if diff := cmp.Diff(want, artifactNames(items)); diff != "" {
		t.Errorf("names (-want +got):\n%s", diff)
	}
	if h.maxInFlight != 4 {
		t.Errorf("listed %d repositories at once, want 4", h.maxInFlight)
	}
}

func TestArtifactsWithSameName(t *testing.T) {
	older, newer := digest("0123456789ab1"), digest("0123456789ab2")
	a := artifactsOf(t, &fakeHarbor{
		repositories: []harbor.Repository{{ID: 1, Name: "proj/app"}},
		artifacts:    map[string][]harbor.Artifact{"proj/app": {{ID: 2, Digest: newer}, {ID: 1, Digest: older}}},
	})
	items := artifactItems(t, a, "ns1", nil)
	if len(items) != 1 || items[0].Status.Digest != older {
		t.Errorf("listed %v, want only %s", items, older)
	}
	obj, err := a.Get(inNamespace("ns1"), "app.sha256-0123456789ab", &metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := obj.(*v1alpha1.HarborArtifact).Status.Digest; got != older {
		t.Errorf("got %s, want %s", got, older)
	}
}

// failingArtifacts fails to list nginx's artifacts, and lists the others only once the poll is cancelled.
type failingArtifacts struct {
	*fakeHarbor
	err       error
	cancelled chan string
}

func (f failingArtifacts) ListArtifacts(ctx context.Context, project, repository string) ([]harbor.Artifact, error) {
	if repository == "nginx" {
		return nil, f.err
	}
	select {
	case <-ctx.Done():
		f.cancelled <- repository
	case <-time.After(5 * time.Second):
	}
	return f.fakeHarbor.ListArtifacts(ctx, project, repository)
}

func TestPollStopsAtFailureOfHarbor(t *testing.T) {
	for _, err := range []error{harbor.ErrUnavailable, harbor.ErrUnauthorized, harbor.ErrForbidden} {
		h := artifactHarbor()
		f := failingArtifacts{h, err, make(chan string, len(h.repositories))}
		s := NewStore(stalenessLimit)
		s.SetNamespace("ns1", true)
		if got := NewPoller(f, "proj", s).Poll(t.Context()); !errors.Is(got, err) {
			t.Errorf("poll returned %v, want %v", got, err)
		}
		if _, err := NewArtifacts(s).List(inNamespace("ns1"), nil); !apierrors.IsServiceUnavailable(err) {
			t.Errorf("got %v, want ServiceUnavailable", err)
		}
		close(f.cancelled)
		var cancelled []string
		for r := range f.cancelled {
			cancelled = append(cancelled, r)
		}
		if diff := cmp.Diff([]string{"dotted.name", "team/api"}, slices.Sorted(slices.Values(cancelled))); diff != "" {
			t.Errorf("%v: cancelled lists (-want +got):\n%s", err, diff)
		}
	}
}

func TestPollKeepsArtifactsOfRepositoryThatFailsUntilStale(t *testing.T) {
	h := artifactHarbor()
	f := newFixture(t, h)
	before := artifactNames(artifactItems(t, f.artifacts, "ns1", nil))

	h.artifactErrs = map[string]error{"proj/nginx": errors.New("more than 1000 pages")}
	h.artifacts["proj/nginx"] = nil
	h.artifacts["proj/team/api"][0].Size++
	f.clock.Step(stalenessLimit / 2)
	f.poll(t)

	items := artifactItems(t, f.artifacts, "ns1", nil)
	if diff := cmp.Diff(before, artifactNames(items)); diff != "" {
		t.Errorf("names (-before +after):\n%s", diff)
	}
	if size := items[3].Status.Size; size != 1537 {
		t.Errorf("team/api artifact has size %d, want the new read's 1537", size)
	}

	f.clock.Step(stalenessLimit / 2)
	f.poll(t)
	expectAvailable(t, f, "ns1")
	f.clock.Step(time.Nanosecond)
	expectUnavailable(t, f, "ns1", "reading a repository's artifacts failed")

	delete(h.artifactErrs, "proj/nginx")
	f.poll(t)
	expectAvailable(t, f, "ns1")
}

func TestNewRepositoryHasNoArtifactsAsOfTheLastRead(t *testing.T) {
	h := artifactHarbor()
	f := newFixture(t, h)
	before := artifactNames(artifactItems(t, f.artifacts, "ns1", nil))
	f.clock.Step(stalenessLimit / 2)
	h.repositories = append(h.repositories, harbor.Repository{ID: 4, Name: "proj/new"})
	h.artifacts["proj/new"] = []harbor.Artifact{{ID: 9, Digest: digest("999999999999")}}
	h.artifactErrs = map[string]error{"proj/new": errors.New("unexpected response from harbor")}
	if err := f.poller.Poll(t.Context()); !errors.Is(err, errArtifactsRead) {
		t.Errorf("poll returned %v, want %v", err, errArtifactsRead)
	}
	expectAvailable(t, f, "ns1")
	if _, err := f.repositories.Get(inNamespace("ns1"), "new", &metav1.GetOptions{}); err != nil {
		t.Error(err)
	}
	if diff := cmp.Diff(before, artifactNames(artifactItems(t, f.artifacts, "ns1", nil))); diff != "" {
		t.Errorf("names (-before +after):\n%s", diff)
	}

	f.clock.Step(stalenessLimit / 2)
	f.poll(t)
	expectAvailable(t, f, "ns1")
	f.clock.Step(time.Nanosecond)
	expectUnavailable(t, f, "ns1", "reading a repository's artifacts failed")
}

func TestFirstReadWithoutSomeArtifactsIsStale(t *testing.T) {
	h := artifactHarbor()
	h.artifactErrs = map[string]error{"proj/nginx": errors.New("unexpected response from harbor")}
	f := newFixture(t, h)
	expectUnavailable(t, f, "ns1", "reading a repository's artifacts failed")
	f.poll(t)
	expectUnavailable(t, f, "ns1", "reading a repository's artifacts failed")

	delete(h.artifactErrs, "proj/nginx")
	f.poll(t)
	expectAvailable(t, f, "ns1")
}

func TestStalenessCountsFromTheStartOfARead(t *testing.T) {
	h := repositoryHarbor()
	f := newFixture(t, h)
	h.onList = func() { f.clock.Step(stalenessLimit / 2) }
	f.poll(t)
	f.clock.Step(stalenessLimit / 2)
	expectAvailable(t, f, "ns1")
	f.clock.Step(time.Nanosecond)
	expectUnavailable(t, f, "ns1", "reading harbor takes longer than the staleness limit")
}

// slowArtifacts lists repository app, and then lists its artifacts until the poll ends, failing with err.
type slowArtifacts struct{ err error }

func (slowArtifacts) ListRepositories(context.Context, string) ([]harbor.Repository, error) {
	return []harbor.Repository{{ID: 1, Name: "proj/app"}}, nil
}

func (s slowArtifacts) ListArtifacts(ctx context.Context, _, _ string) ([]harbor.Artifact, error) {
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
	}
	return nil, s.err
}

func TestPollEndsAtTheStalenessLimit(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("%w: %w", harbor.ErrUnavailable, context.DeadlineExceeded),
		fmt.Errorf("decoding a response: %w", context.DeadlineExceeded),
	} {
		s := NewStore(50 * time.Millisecond)
		s.SetNamespace("ns1", true)
		if got := NewPoller(slowArtifacts{err}, "proj", s).Poll(t.Context()); !errors.Is(got, errSlowRead) {
			t.Errorf("%v: poll returned %v, want %v", err, got, errSlowRead)
		}
		_, listErr := NewRepositories(s).List(inNamespace("ns1"), nil)
		if !apierrors.IsServiceUnavailable(listErr) || !strings.Contains(listErr.Error(), "reading harbor takes longer than the staleness limit") {
			t.Errorf("%v: list returned %v", err, listErr)
		}
	}
}

func TestPollsDoNotOverlap(t *testing.T) {
	h := repositoryHarbor()
	f := newUnreadFixture(h)
	listing, release := make(chan struct{}, 2), make(chan struct{})
	h.onList = func() {
		listing <- struct{}{}
		<-release
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { f.poll(t) })
	}
	<-listing
	time.Sleep(50 * time.Millisecond)
	if len(listing) > 0 {
		t.Error("listed repositories twice at once")
	}
	close(release)
	wg.Wait()
}

// stepWhenWaiting steps the clock once something waits on it.
func stepWhenWaiting(t *testing.T, f *fixture, d time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !f.clock.HasWaiters(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("nothing waits on the clock")
		}
	}
	f.clock.Step(d)
}

func expectRepositoryLists(t *testing.T, h *fakeHarbor, want int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); h.repositoryLists() != want; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("listed repositories %d times, want %d", h.repositoryLists(), want)
		}
	}
}

func TestRunRetriesOnceSoonerAfterAFailure(t *testing.T) {
	h := repositoryHarbor()
	h.setErr(harbor.ErrUnavailable)
	f := newUnreadFixture(h)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() { defer close(done); f.poller.Run(ctx, time.Minute) }()

	select {
	case <-f.poller.Attempted():
	case <-time.After(5 * time.Second):
		t.Fatal("no read attempted")
	}
	expectRepositoryLists(t, h, 1)
	stepWhenWaiting(t, f, 1100*time.Millisecond)
	expectRepositoryLists(t, h, 2)
	stepWhenWaiting(t, f, 59*time.Second)
	time.Sleep(10 * time.Millisecond)
	expectRepositoryLists(t, h, 2)

	h.setErr(nil)
	stepWhenWaiting(t, f, 7*time.Second)
	expectRepositoryLists(t, h, 3)
	stepWhenWaiting(t, f, 59*time.Second)
	time.Sleep(10 * time.Millisecond)
	expectRepositoryLists(t, h, 3)
	expectAvailable(t, f, "ns1")

	h.setErr(harbor.ErrUnavailable)
	stepWhenWaiting(t, f, 7*time.Second)
	expectRepositoryLists(t, h, 4)
	stepWhenWaiting(t, f, 1100*time.Millisecond)
	expectRepositoryLists(t, h, 5)
}
