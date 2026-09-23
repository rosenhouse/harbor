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
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
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
		s := NewStore("proj", stalenessLimit, fakeNamespaces{"ns1"})
		if got := NewPoller(f, s).Poll(t.Context()); !errors.Is(got, err) {
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

// artifactRequests are requests for artifacts in ns1, and whether they could return nginx's.
var artifactRequests = []struct {
	desc  string
	nginx bool
	send  func(*Artifacts) error
}{
	{"get of an nginx artifact", true, getArtifact("nginx.sha256-111111111111")},
	{"get of a team/api artifact", false, getArtifact("team.api.sha256-aaaaaaaaaaaa")},
	{"get of a dotted.name artifact", false, getArtifact(dottedArtifact)},
	{"list", true, listArtifacts(nil)},
	{"list by label inequality", true, listArtifacts(notTeamAPI)},
	{"list by nginx label", true, listArtifacts(byLabel("nginx"))},
	{"list by team.api label", false, listArtifacts(byLabel("team.api"))},
	{"list by nginx repository", true, listArtifacts(byRepository("proj/nginx"))},
	{"list by team/api repository", false, listArtifacts(byRepository("proj/team/api"))},
	{"list by nginx artifact name", true, listArtifacts(byName("nginx.sha256-111111111111"))},
	{"list by team/api artifact name", false, listArtifacts(byName("team.api.sha256-aaaaaaaaaaaa"))},
}

func getArtifact(name string) func(*Artifacts) error {
	return func(a *Artifacts) error {
		_, err := a.Get(inNamespace("ns1"), name, &metav1.GetOptions{})
		return err
	}
}

func listArtifacts(opts *metainternalversion.ListOptions) func(*Artifacts) error {
	return func(a *Artifacts) error {
		_, err := a.List(inNamespace("ns1"), opts)
		return err
	}
}

// expectArtifactRequests checks that requests fail if and only if they could return nginx's artifacts and those are stale.
func expectArtifactRequests(t *testing.T, f *fixture, nginxStale bool) {
	t.Helper()
	for _, r := range artifactRequests {
		err := r.send(f.artifacts)
		switch {
		case !r.nginx || !nginxStale:
			if err != nil {
				t.Errorf("%s: %v", r.desc, err)
			}
		case !apierrors.IsServiceUnavailable(err) || !strings.Contains(err.Error(), "reading a repository's artifacts failed"):
			t.Errorf("%s: got %v, want ServiceUnavailable", r.desc, err)
		}
	}
}

func TestPollKeepsArtifactsOfRepositoryThatFailsUntilStale(t *testing.T) {
	h := artifactHarbor()
	f := newFixture(t, h)
	before := artifactNames(artifactItems(t, f.artifacts, "ns1", nil))

	h.artifactErrs = map[string]error{"proj/nginx": errors.New("more than 1000 pages")}
	h.artifacts["proj/team/api"][0].Size++
	f.clock.Step(stalenessLimit / 2)
	if err := f.poller.Poll(t.Context()); !errors.Is(err, errArtifactsRead) {
		t.Errorf("poll returned %v, want %v", err, errArtifactsRead)
	}

	items := artifactItems(t, f.artifacts, "ns1", nil)
	if diff := cmp.Diff(before, artifactNames(items)); diff != "" {
		t.Errorf("names (-before +after):\n%s", diff)
	}
	if size := items[3].Status.Size; size != 1537 {
		t.Errorf("team/api artifact has size %d, want the new poll's 1537", size)
	}

	f.clock.Step(stalenessLimit / 2)
	f.poll(t)
	expectArtifactRequests(t, f, false)
	f.clock.Step(time.Nanosecond)
	expectArtifactRequests(t, f, true)
	expectAvailable(t, f, "ns1")

	delete(h.artifactErrs, "proj/nginx")
	f.poll(t)
	expectArtifactRequests(t, f, false)
}

func TestNewRepositoryHasNoArtifactsAsOfTheLastPoll(t *testing.T) {
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
	if _, err := f.repositories.Get(inNamespace("ns1"), "new", &metav1.GetOptions{}); err != nil {
		t.Error(err)
	}
	if diff := cmp.Diff(before, artifactNames(artifactItems(t, f.artifacts, "ns1", nil))); diff != "" {
		t.Errorf("names (-before +after):\n%s", diff)
	}

	f.clock.Step(stalenessLimit / 2)
	f.poll(t)
	expectArtifactRequests(t, f, false)
	f.clock.Step(time.Nanosecond)
	expectAvailable(t, f, "ns1")
	for _, send := range []func(*Artifacts) error{getArtifact("new.sha256-999999999999"), listArtifacts(nil)} {
		if err := send(f.artifacts); !apierrors.IsServiceUnavailable(err) {
			t.Errorf("got %v, want ServiceUnavailable", err)
		}
	}
	if err := getArtifact("team.api.sha256-aaaaaaaaaaaa")(f.artifacts); err != nil {
		t.Error(err)
	}
}

func TestFirstPollWithoutSomeArtifactsFailsRequestsForThem(t *testing.T) {
	h := artifactHarbor()
	h.artifactErrs = map[string]error{"proj/nginx": errors.New("unexpected response from harbor")}
	f := newFixture(t, h)
	expectArtifactRequests(t, f, true)
	expectAvailable(t, f, "ns1")
	f.poll(t)
	expectArtifactRequests(t, f, true)

	delete(h.artifactErrs, "proj/nginx")
	f.poll(t)
	expectArtifactRequests(t, f, false)
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
		s := NewStore("proj", 50*time.Millisecond, fakeNamespaces{"ns1"})
		if got := NewPoller(slowArtifacts{err}, s).Poll(t.Context()); !errors.Is(got, errSlowRead) {
			t.Errorf("%v: poll returned %v, want %v", err, got, errSlowRead)
		}
		_, listErr := NewRepositories(s).List(inNamespace("ns1"), nil)
		if !apierrors.IsServiceUnavailable(listErr) || !strings.Contains(listErr.Error(), "reading harbor takes longer than the staleness limit") {
			t.Errorf("%v: list returned %v", err, listErr)
		}
	}
}

func TestPollAtShutdownRecordsNoFailure(t *testing.T) {
	f := newUnreadFixture(repositoryHarbor())
	f.harbor.setErr(context.Canceled)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_ = f.poller.Poll(ctx)
	expectUnavailable(t, f, "ns1", "harbor has not been read yet")
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

// waitForWaiter waits until something waits on the clock.
func waitForWaiter(t *testing.T, f *fixture) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !f.clock.HasWaiters(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("nothing waits on the clock")
		}
	}
}

func expectRepositoryLists(t *testing.T, h *fakeHarbor, want int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); h.repositoryLists() != want; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("listed repositories %d times, want %d", h.repositoryLists(), want)
		}
	}
}

// expectWaiting checks that Run waits on the clock, and has listed repositories lists times.
// Run lists repositories before it waits again, so this sees any poll that the clock started.
func expectWaiting(t *testing.T, f *fixture, lists int) {
	t.Helper()
	if !f.clock.HasWaiters() || f.harbor.repositoryLists() != lists {
		t.Fatalf("waiting %v after listing repositories %d times, want waiting after %d", f.clock.HasWaiters(), f.harbor.repositoryLists(), lists)
	}
}

func TestRunRetriesOnceSoonerAfterAFailure(t *testing.T) {
	for _, tc := range []struct{ interval, retry time.Duration }{
		{time.Minute, time.Second},
		{100 * time.Millisecond, 100 * time.Millisecond},
	} {
		h := repositoryHarbor()
		h.setErr(harbor.ErrUnavailable)
		f := newUnreadFixture(h)
		f.poller.jitter = func(d time.Duration) time.Duration { return d + d/10 }
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		t.Cleanup(func() { cancel(); <-done })
		go func() { defer close(done); f.poller.Run(ctx, tc.interval) }()

		// waitThenPoll checks that Run waits d plus a tenth, and then polls.
		polls := 1
		waitThenPoll := func(d time.Duration) {
			t.Helper()
			f.clock.Step(d)
			expectWaiting(t, f, polls)
			f.clock.Step(d / 10)
			polls++
			expectRepositoryLists(t, h, polls)
			waitForWaiter(t, f)
		}
		expectRepositoryLists(t, h, 1)
		waitForWaiter(t, f)
		waitThenPoll(tc.retry)
		h.setErr(nil)
		waitThenPoll(tc.interval)
		waitThenPoll(tc.interval)
		expectAvailable(t, f, "ns1")
		h.setErr(harbor.ErrUnavailable)
		waitThenPoll(tc.interval)
		waitThenPoll(tc.retry)
		waitThenPoll(tc.interval)
	}
}

func TestJitterAddsUpToATenth(t *testing.T) {
	jitter := NewPoller(nil, nil).jitter
	lengthened := false
	for range 100 {
		d := jitter(time.Minute)
		if d < time.Minute || d > time.Minute+time.Minute/10 {
			t.Fatalf("jittered a minute to %v", d)
		}
		lengthened = lengthened || d > time.Minute
	}
	if !lengthened {
		t.Error("jitter never lengthened a minute")
	}
}
