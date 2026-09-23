package registry

import (
	"context"
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/watch"
	apistorage "k8s.io/apiserver/pkg/storage"
	"k8s.io/utils/ptr"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

func from(rv string) *metainternalversion.ListOptions {
	return &metainternalversion.ListOptions{ResourceVersion: rv}
}

func objectRV(t *testing.T, e watch.Event) string {
	t.Helper()
	o, err := meta.Accessor(e.Object)
	if err != nil {
		t.Fatal(err)
	}
	return o.GetResourceVersion()
}

func TestPollRecordsChanges(t *testing.T) {
	f := newFixture(t, &fakeHarbor{repositories: []harbor.Repository{
		{ID: 1, Name: "proj/unchanged"},
		{ID: 2, Name: "proj/modified"},
		{ID: 3, Name: "proj/deleted"},
		{ID: 4, Name: "proj/recreated"},
	}})
	w := startWatch(t, f.repositories, "ns1", from(resourceVersion(t, f.repositories)))

	f.harbor.repositories = []harbor.Repository{
		{ID: 1, Name: "proj/unchanged"},
		{ID: 2, Name: "proj/modified", PullCount: 1},
		{ID: 5, Name: "proj/recreated"},
		{ID: 6, Name: "proj/added"},
	}
	f.poll(t)

	want := []string{"ADDED ns1/added", "DELETED ns1/deleted", "MODIFIED ns1/modified", "DELETED ns1/recreated", "ADDED ns1/recreated"}
	if diff := cmp.Diff(want, nextEvents(t, w, len(want))); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
	f.poll(t)
	expectNoEvent(t, w)
	if _, err := f.repositories.Get(inNamespace("ns1"), "deleted", &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("get deleted: got %v, want NotFound", err)
	}
}

// expectListedAsWatched fails unless each event's object is listed with the event's resourceVersion.
func expectListedAsWatched(t *testing.T, f *fixture, events []watch.Event) {
	t.Helper()
	listed := map[string]string{}
	for _, r := range listItems(t, f.repositories, "", nil) {
		listed[r.Namespace+"/"+r.Name] = r.ResourceVersion
	}
	for _, e := range events {
		o := e.Object.(*v1alpha1.HarborRepository)
		if rv := listed[o.Namespace+"/"+o.Name]; rv != o.ResourceVersion {
			t.Errorf("%s: listed with resourceVersion %s, watched with %s", describe(e), rv, o.ResourceVersion)
		}
	}
}

func TestResourceVersions(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	w := startWatch(t, f.repositories, "", from(resourceVersion(t, f.repositories)))
	before := listItems(t, f.repositories, "", nil)

	f.harbor.repositories[0].PullCount++
	f.poll(t)

	events := []watch.Event{nextEvent(t, w), nextEvent(t, w)}
	if first, second := parseRV(t, objectRV(t, events[0])), parseRV(t, objectRV(t, events[1])); second <= first {
		t.Errorf("event resourceVersions %d, %d do not increase", first, second)
	}
	if listed := resourceVersion(t, f.repositories); listed != objectRV(t, events[1]) {
		t.Errorf("list has resourceVersion %s, want the last event's %s", listed, objectRV(t, events[1]))
	}
	expectListedAsWatched(t, f, events)
	after := listItems(t, f.repositories, "", nil)
	for i := range after {
		b, a := before[i], after[i]
		switch changed := a.Name == "nginx"; {
		case changed && parseRV(t, a.ResourceVersion) <= parseRV(t, b.ResourceVersion):
			t.Errorf("%s/%s changed but kept resourceVersion %s", a.Namespace, a.Name, a.ResourceVersion)
		case !changed && a.ResourceVersion != b.ResourceVersion:
			t.Errorf("%s/%s did not change but its resourceVersion changed from %s to %s", a.Namespace, a.Name, b.ResourceVersion, a.ResourceVersion)
		}
	}

	rv := resourceVersion(t, f.repositories)
	f.poll(t)
	if again := resourceVersion(t, f.repositories); again != rv {
		t.Errorf("a poll without changes moved the resourceVersion from %s to %s", rv, again)
	}
}

func TestWatchResumesFromAnyEvent(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	all := startWatch(t, f.repositories, "", from(resourceVersion(t, f.repositories)))
	f.harbor.repositories[0].PullCount++
	f.poll(t)
	f.store.SetNamespace("ns3", true)
	f.harbor.repositories = f.harbor.repositories[1:]
	f.poll(t)

	var events []watch.Event
	var described []string
	for range 8 {
		e := nextEvent(t, all)
		if n := len(events); n > 0 && parseRV(t, objectRV(t, e)) <= parseRV(t, objectRV(t, events[n-1])) {
			t.Errorf("%s at %s follows %s at %s", describe(e), objectRV(t, e), describe(events[n-1]), objectRV(t, events[n-1]))
		}
		events = append(events, e)
		described = append(described, describe(e))
	}
	for i, e := range events {
		w := startWatch(t, f.repositories, "", from(objectRV(t, e)))
		if want := described[i+1:]; len(want) > 0 {
			if diff := cmp.Diff(want, nextEvents(t, w, len(want))); diff != "" {
				t.Errorf("from %s: events (-want +got):\n%s", described[i], diff)
			}
		}
		expectNoEvent(t, w)
	}
}

func TestWatchFromUnknownResourceVersion(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	f.store.logSize = 1
	start := resourceVersion(t, f.repositories)
	f.harbor.repositories[0].PullCount++
	f.poll(t)
	kept := resourceVersion(t, f.repositories)
	f.harbor.repositories[1].PullCount++
	f.poll(t)
	next := strconv.FormatUint(parseRV(t, resourceVersion(t, f.repositories))+1, 10)

	startWatch(t, f.repositories, "", from(kept))
	for _, rv := range []string{start, next} {
		if _, err := f.repositories.Watch(inNamespace(""), from(rv)); !apierrors.IsResourceExpired(err) {
			t.Errorf("watch from %s: got %v, want Expired", rv, err)
		}
	}
	if _, err := f.repositories.Watch(inNamespace(""), from("x")); !apierrors.IsBadRequest(err) {
		t.Errorf("watch from x: got %v, want BadRequest", err)
	}
}

func TestWatcherThatFallsBehindExpires(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	f.store.logSize = 1
	w := startWatch(t, f.repositories, "", from(resourceVersion(t, f.repositories)))
	f.harbor.repositories[0].PullCount++
	f.poll(t)
	// The watcher has read the change once it sends its first event.
	nextEvents(t, w, 1)
	for range 2 {
		f.harbor.repositories[1].PullCount++
		f.poll(t)
	}

	if diff := cmp.Diff([]string{"MODIFIED ns2/nginx", "ERROR Expired"}, nextEvents(t, w, 2)); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
	expectEnd(t, w)
}

func TestCaughtUpWatchSurvivesLargeNamespaceChange(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	f.store.logSize = 2
	w := startWatch(t, f.repositories, "ns1", from(resourceVersion(t, f.repositories)))
	f.store.SetNamespace("ns3", true)
	expectNoEvent(t, w)
}

func TestCaughtUpWatchSurvivesLargePoll(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	f.store.logSize = 2
	w := startWatch(t, f.repositories, "ns1", from(resourceVersion(t, f.repositories)))
	for i := range f.harbor.repositories {
		f.harbor.repositories[i].PullCount++
	}
	f.poll(t)
	want := []string{"MODIFIED ns1/" + dottedRepository, "MODIFIED ns1/nginx", "MODIFIED ns1/team.api"}
	if diff := cmp.Diff(want, nextEvents(t, w, len(want))); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
}

func TestLogCountsEachHarborChangeOnce(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	f.store.logSize = 2
	start := resourceVersion(t, f.repositories)
	f.harbor.repositories[0].PullCount++
	f.poll(t)
	f.harbor.repositories[1].PullCount++
	f.poll(t)

	w := startWatch(t, f.repositories, "", from(start))
	want := []string{"MODIFIED ns1/nginx", "MODIFIED ns2/nginx", "MODIFIED ns1/team.api", "MODIFIED ns2/team.api"}
	if diff := cmp.Diff(want, nextEvents(t, w, len(want))); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
}

func TestLogCountsANamespaceChangeOnceForEachObject(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	f.store.logSize = len(f.harbor.repositories)
	start := resourceVersion(t, f.repositories)
	f.store.SetNamespace("ns3", true)
	f.harbor.repositories[0].PullCount++
	f.poll(t)

	if _, err := f.repositories.Watch(inNamespace(""), from(start)); !apierrors.IsResourceExpired(err) {
		t.Errorf("watch from before the namespace change: got %v, want Expired", err)
	}
}

func TestWatchStartsWithCurrentState(t *testing.T) {
	for _, opts := range []*metainternalversion.ListOptions{
		from(""),
		from("0"),
		{SendInitialEvents: ptr.To(true), ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan},
	} {
		f := newFixture(t, repositoryHarbor())
		w := startWatch(t, f.repositories, "ns2", opts)
		want := []string{"ADDED ns2/" + dottedRepository, "ADDED ns2/nginx", "ADDED ns2/team.api"}
		if diff := cmp.Diff(want, nextEvents(t, w, len(want))); diff != "" {
			t.Errorf("%+v: initial events (-want +got):\n%s", opts, diff)
		}
		f.harbor.repositories = f.harbor.repositories[1:]
		f.poll(t)
		if diff := cmp.Diff([]string{"DELETED ns2/nginx"}, nextEvents(t, w, 1)); diff != "" {
			t.Errorf("%+v: later events (-want +got):\n%s", opts, diff)
		}
	}
}

func TestWatchFromNowSkipsCurrentState(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	w := startWatch(t, f.repositories, "ns2", &metainternalversion.ListOptions{SendInitialEvents: ptr.To(false), ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan})
	f.harbor.repositories = f.harbor.repositories[1:]
	f.poll(t)
	if diff := cmp.Diff([]string{"DELETED ns2/nginx"}, nextEvents(t, w, 1)); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
}

func TestWatchMarksEndOfInitialEvents(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	rv := resourceVersion(t, f.repositories)
	w := startWatch(t, f.repositories, "ns1", &metainternalversion.ListOptions{
		SendInitialEvents:    ptr.To(true),
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
		AllowWatchBookmarks:  true,
	})
	nextEvents(t, w, 3)
	e := nextEvent(t, w)
	bookmark, ok := e.Object.(*v1alpha1.HarborRepository)
	if e.Type != watch.Bookmark || !ok {
		t.Fatalf("got %s, want a HarborRepository bookmark", describe(e))
	}
	if bookmark.ResourceVersion != rv || bookmark.Annotations[metav1.InitialEventsAnnotationKey] != "true" {
		t.Errorf("bookmark has resourceVersion %s and annotations %v, want %s and the end of initial events", bookmark.ResourceVersion, bookmark.Annotations, rv)
	}
}

func TestWatchDoesNotMarkEndOfStaleInitialEvents(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	w := startWatch(t, f.repositories, "ns1", &metainternalversion.ListOptions{
		SendInitialEvents:    ptr.To(true),
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
		AllowWatchBookmarks:  true,
	})
	f.clock.Step(stalenessLimit + time.Nanosecond)
	want := []string{"ADDED ns1/" + dottedRepository, "ADDED ns1/nginx", "ADDED ns1/team.api", "ERROR ServiceUnavailable"}
	if diff := cmp.Diff(want, nextEvents(t, w, len(want))); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
	expectEnd(t, w)
}

// bookmarkAfterInterval steps the clock past the bookmark interval once w's ticker is waiting.
func bookmarkAfterInterval(t *testing.T, f *fixture) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !f.clock.HasWaiters(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("watch has no bookmark ticker")
		}
	}
	f.clock.Step(f.store.bookmarkInterval)
}

func TestWatchSendsBookmarks(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	f.store.bookmarkInterval = stalenessLimit / 4
	opts := &metainternalversion.ListOptions{
		ResourceVersion:     resourceVersion(t, f.repositories),
		FieldSelector:       fields.OneTermEqualSelector("metadata.name", "team.api"),
		AllowWatchBookmarks: true,
	}
	w := startWatch(t, f.repositories, "", opts)
	f.harbor.repositories[0].PullCount++
	f.poll(t)
	rv := resourceVersion(t, f.repositories)

	for range 2 {
		bookmarkAfterInterval(t, f)
		e := nextEvent(t, w)
		if o := e.Object.(*v1alpha1.HarborRepository); e.Type != watch.Bookmark || o.ResourceVersion != rv || o.Name != "" {
			t.Errorf("got %s at resourceVersion %s, want a bookmark at %s", describe(e), o.ResourceVersion, rv)
		}
	}
}

func TestWatchSendsBookmarksOnlyWhenAllowed(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	w := startWatch(t, f.repositories, "ns1", from(resourceVersion(t, f.repositories)))
	f.harbor.repositories[0].PullCount++
	f.poll(t)
	// The watch skips the change in ns2, which a bookmark would report.
	nextEvents(t, w, 1)
	f.clock.Step(f.store.bookmarkInterval)
	expectNoEvent(t, w)
}

func TestWatchSeesOnlyItsResource(t *testing.T) {
	f := newFixture(t, artifactHarbor())
	rv := resourceVersion(t, f.repositories)
	repositories := startWatch(t, f.repositories, "ns1", from(rv))
	artifacts := startWatch(t, f.artifacts, "ns1", from(rv))
	f.harbor.repositories[0].PullCount++
	f.harbor.artifacts["proj/nginx"][0].Size++
	f.poll(t)

	if diff := cmp.Diff([]string{"MODIFIED ns1/team.api"}, nextEvents(t, repositories, 1)); diff != "" {
		t.Errorf("repository events (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"MODIFIED ns1/nginx.sha256-111111111111"}, nextEvents(t, artifacts, 1)); diff != "" {
		t.Errorf("artifact events (-want +got):\n%s", diff)
	}
	expectNoEvent(t, repositories)
	expectNoEvent(t, artifacts)
}

func TestWatchSelectors(t *testing.T) {
	byFields := func(set fields.Set) *metainternalversion.ListOptions {
		return &metainternalversion.ListOptions{FieldSelector: fields.SelectorFromSet(set)}
	}
	for _, tc := range []struct {
		desc, namespace string
		opts            *metainternalversion.ListOptions
		want            []string
	}{
		{"namespace", "ns2", &metainternalversion.ListOptions{}, []string{"ns2/" + dottedArtifact, "ns2/nginx.sha256-111111111111", "ns2/nginx.sha256-222222222222", "ns2/team.api.sha256-aaaaaaaaaaaa"}},
		{"namespace field", "", byFields(fields.Set{"metadata.namespace": "ns1"}), []string{"ns1/" + dottedArtifact, "ns1/nginx.sha256-111111111111", "ns1/nginx.sha256-222222222222", "ns1/team.api.sha256-aaaaaaaaaaaa"}},
		{"name", "", byFields(fields.Set{"metadata.name": "nginx.sha256-111111111111"}), []string{"ns1/nginx.sha256-111111111111", "ns2/nginx.sha256-111111111111"}},
		{"repository", "ns1", byFields(fields.Set{"status.repository": "proj/nginx"}), []string{"ns1/nginx.sha256-111111111111", "ns1/nginx.sha256-222222222222"}},
		{"label", "ns1", &metainternalversion.ListOptions{LabelSelector: labels.SelectorFromSet(labels.Set{v1alpha1.RepositoryLabel: "team.api"})}, []string{"ns1/team.api.sha256-aaaaaaaaaaaa"}},
	} {
		f := newFixture(t, artifactHarbor())
		tc.opts.ResourceVersion = resourceVersion(t, f.artifacts)
		w := startWatch(t, f.artifacts, tc.namespace, tc.opts)
		for _, artifacts := range f.harbor.artifacts {
			for i := range artifacts {
				artifacts[i].Size++
			}
		}
		f.poll(t)

		var want []string
		for _, name := range tc.want {
			want = append(want, "MODIFIED "+name)
		}
		if diff := cmp.Diff(want, nextEvents(t, w, len(want))); diff != "" {
			t.Errorf("%s: events (-want +got):\n%s", tc.desc, diff)
		}
		expectNoEvent(t, w)
	}
}

// colored returns a read of Harbor with one repository, app, whose object has a color label.
func colored(color string) map[key]*item {
	obj := &v1alpha1.HarborRepository{ObjectMeta: metav1.ObjectMeta{Name: "app", Labels: map[string]string{"color": color}}}
	return map[key]*item{{repositoriesResource.Resource, "app"}: {harborID: 1, obj: obj}}
}

func TestObjectsThatStartOrStopMatchingAppearOrDisappear(t *testing.T) {
	f := newUnreadFixture(&fakeHarbor{})
	f.store.update(colored("red"), f.clock.Now(), nil)
	rv := resourceVersion(t, f.repositories)
	byColor := func(color string) *metainternalversion.ListOptions {
		return &metainternalversion.ListOptions{ResourceVersion: rv, LabelSelector: labels.SelectorFromSet(labels.Set{"color": color})}
	}
	red := startWatch(t, f.repositories, "ns1", byColor("red"))
	blue := startWatch(t, f.repositories, "ns1", byColor("blue"))
	f.store.update(colored("blue"), f.clock.Now(), nil)

	deleted, added := nextEvent(t, red), nextEvent(t, blue)
	if diff := cmp.Diff([]string{"DELETED ns1/app", "ADDED ns1/app"}, []string{describe(deleted), describe(added)}); diff != "" {
		t.Errorf("red and blue watch events (-want +got):\n%s", diff)
	}
	if o := deleted.Object.(*v1alpha1.HarborRepository); o.Labels["color"] != "red" || o.ResourceVersion != objectRV(t, added) {
		t.Errorf("deleted object has labels %v and resourceVersion %s, want the red object at the change's %s", o.Labels, o.ResourceVersion, objectRV(t, added))
	}
}

func TestNamespaceChangesAreEvents(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	all := startWatch(t, f.repositories, "", from(resourceVersion(t, f.repositories)))
	ns3 := startWatch(t, f.repositories, "ns3", from("0"))

	f.store.SetNamespace("ns3", true)
	f.store.SetNamespace("ns3", true)
	added := []string{"ADDED ns3/" + dottedRepository, "ADDED ns3/nginx", "ADDED ns3/team.api"}
	var events []watch.Event
	for range 3 {
		events = append(events, nextEvent(t, all))
	}
	if diff := cmp.Diff(added, []string{describe(events[0]), describe(events[1]), describe(events[2])}); diff != "" {
		t.Errorf("all namespaces: events (-want +got):\n%s", diff)
	}
	expectListedAsWatched(t, f, events)
	if diff := cmp.Diff(added, nextEvents(t, ns3, 3)); diff != "" {
		t.Errorf("ns3: events (-want +got):\n%s", diff)
	}

	f.store.SetNamespace("ns1", false)
	f.store.SetNamespace("other", false)
	want := []string{"DELETED ns1/" + dottedRepository, "DELETED ns1/nginx", "DELETED ns1/team.api"}
	if diff := cmp.Diff(want, nextEvents(t, all, 3)); diff != "" {
		t.Errorf("all namespaces: events (-want +got):\n%s", diff)
	}
	expectNoEvent(t, all)
	expectNoEvent(t, ns3)

	if diff := cmp.Diff([]string{"ns2/" + dottedRepository, "ns2/nginx", "ns2/team.api", "ns3/" + dottedRepository, "ns3/nginx", "ns3/team.api"}, names(listItems(t, f.repositories, "", nil))); diff != "" {
		t.Errorf("listed (-want +got):\n%s", diff)
	}
	if _, err := f.repositories.Get(inNamespace("ns1"), "nginx", &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("get from ns1: got %v, want NotFound", err)
	}
}

func TestListResourceVersion(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	old := resourceVersion(t, f.repositories)
	f.harbor.repositories[0].PullCount++
	f.poll(t)
	current := resourceVersion(t, f.repositories)
	next := strconv.FormatUint(parseRV(t, current)+1, 10)
	succeeds := func(err error) bool { return err == nil }

	for _, tc := range []struct {
		rv    string
		match metav1.ResourceVersionMatch
		want  func(error) bool
	}{
		{"0", "", succeeds},
		{current, metav1.ResourceVersionMatchExact, succeeds},
		{old, metav1.ResourceVersionMatchExact, apierrors.IsResourceExpired},
		{next, metav1.ResourceVersionMatchExact, apistorage.IsTooLargeResourceVersion},
		{old, metav1.ResourceVersionMatchNotOlderThan, succeeds},
		{next, metav1.ResourceVersionMatchNotOlderThan, apistorage.IsTooLargeResourceVersion},
		{next, "", apistorage.IsTooLargeResourceVersion},
		{"x", "", apierrors.IsBadRequest},
		{"00", "", apierrors.IsBadRequest},
		{"0" + current, "", apierrors.IsBadRequest},
	} {
		_, err := f.repositories.List(inNamespace("ns1"), &metainternalversion.ListOptions{ResourceVersion: tc.rv, ResourceVersionMatch: tc.match})
		if !tc.want(err) {
			t.Errorf("list with resourceVersion %s, match %q: got %v", tc.rv, tc.match, err)
		}
	}
}

func TestGetResourceVersion(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	current := resourceVersion(t, f.repositories)
	next := strconv.FormatUint(parseRV(t, current)+1, 10)
	succeeds := func(err error) bool { return err == nil }
	for _, tc := range []struct {
		rv   string
		want func(error) bool
	}{
		{"", succeeds},
		{"0", succeeds},
		{current, succeeds},
		{next, apistorage.IsTooLargeResourceVersion},
		{"x", apierrors.IsBadRequest},
		{"00", apierrors.IsBadRequest},
	} {
		_, err := f.repositories.Get(inNamespace("ns1"), "nginx", &metav1.GetOptions{ResourceVersion: tc.rv})
		if !tc.want(err) {
			t.Errorf("get with resourceVersion %q: got %v", tc.rv, err)
		}
	}
}

func TestWatchFromNonCanonicalZero(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	if _, err := f.repositories.Watch(inNamespace("ns1"), from("00")); !apierrors.IsBadRequest(err) {
		t.Errorf("got %v, want BadRequest", err)
	}
}

func TestInitialEventsResourceVersion(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	current := resourceVersion(t, f.repositories)
	initialFrom := func(rv string) *metainternalversion.ListOptions {
		return &metainternalversion.ListOptions{ResourceVersion: rv, SendInitialEvents: ptr.To(true), ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan}
	}
	startWatch(t, f.repositories, "ns1", initialFrom(current))
	if _, err := f.repositories.Watch(inNamespace("ns1"), initialFrom(strconv.FormatUint(parseRV(t, current)+1, 10))); !apistorage.IsTooLargeResourceVersion(err) {
		t.Errorf("from a later resourceVersion: got %v, want too large", err)
	}
	if _, err := f.repositories.Watch(inNamespace("ns1"), initialFrom("x")); !apierrors.IsBadRequest(err) {
		t.Errorf("from x: got %v, want BadRequest", err)
	}
}

// requests returns the errors of a get, a list, and a watch of repositories in namespace.
func requests(f *fixture, namespace string) []error {
	_, getErr := f.repositories.Get(inNamespace(namespace), "nginx", &metav1.GetOptions{})
	_, listErr := f.repositories.List(inNamespace(namespace), nil)
	w, watchErr := f.repositories.Watch(inNamespace(namespace), from("0"))
	if watchErr == nil {
		w.Stop()
	}
	return []error{getErr, listErr, watchErr}
}

func expectUnavailable(t *testing.T, f *fixture, namespace, message string) {
	t.Helper()
	for i, err := range requests(f, namespace) {
		if !apierrors.IsServiceUnavailable(err) || !strings.Contains(err.Error(), message) {
			t.Errorf("%s in %q: got %v, want ServiceUnavailable: %s", []string{"get", "list", "watch"}[i], namespace, err, message)
		}
	}
}

func expectAvailable(t *testing.T, f *fixture, namespace string) {
	t.Helper()
	for i, err := range requests(f, namespace) {
		if err != nil {
			t.Errorf("%s in %q: %v", []string{"get", "list", "watch"}[i], namespace, err)
		}
	}
}

func TestUnavailableUntilFirstRead(t *testing.T) {
	f := newUnreadFixture(repositoryHarbor())
	expectUnavailable(t, f, "ns1", "harbor has not been read yet")
	if _, err := f.repositories.List(inNamespace(""), nil); !apierrors.IsServiceUnavailable(err) {
		t.Errorf("list from all namespaces: got %v, want ServiceUnavailable", err)
	}

	f.harbor.err = fmt.Errorf("%w: dial tcp 10.0.0.1:443: refused", harbor.ErrUnavailable)
	f.poll(t)
	expectUnavailable(t, f, "ns1", "harbor is unavailable")

	_, err := f.repositories.Get(inNamespace("other"), "nginx", &metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("get from a namespace that does not see the project: got %v, want NotFound", err)
	}
	if n := names(listItems(t, f.repositories, "other", nil)); len(n) != 0 {
		t.Errorf("list from a namespace that does not see the project: got %v", n)
	}
	startWatch(t, f.repositories, "other", from("0"))

	f.harbor.err = nil
	f.poll(t)
	expectAvailable(t, f, "ns1")
}

func TestServesLastReadUntilStale(t *testing.T) {
	for _, tc := range []struct {
		harborErr error
		want      string
	}{
		{fmt.Errorf("%w: dial tcp 10.0.0.1:443: refused", harbor.ErrUnavailable), "harbor is unavailable"},
		{fmt.Errorf("%w: GET https://10.0.0.1/api/v2.0/projects/proj/repositories: 401", harbor.ErrUnauthorized), "harbor rejected the robot account credentials"},
		{harbor.ErrForbidden, "harbor denied the robot account access"},
		{harbor.ErrNotFound, "not found in harbor"},
		{fmt.Errorf("boom from 10.0.0.1"), "unexpected error reading harbor"},
	} {
		f := newFixture(t, repositoryHarbor())
		f.harbor.err = tc.harborErr
		f.clock.Step(stalenessLimit / 2)
		f.poll(t)
		f.clock.Step(stalenessLimit / 2)
		expectAvailable(t, f, "ns1")

		f.clock.Step(time.Nanosecond)
		expectUnavailable(t, f, "ns1", tc.want)
		for _, err := range requests(f, "ns1") {
			if strings.Contains(err.Error(), "10.0.0.1") {
				t.Errorf("%v: error %q leaks details", tc.harborErr, err)
			}
		}

		f.harbor.err = nil
		f.poll(t)
		expectAvailable(t, f, "ns1")
	}
}

func TestStaleWithoutFailedRead(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	f.clock.Step(stalenessLimit + time.Nanosecond)
	expectUnavailable(t, f, "ns1", "reading harbor takes longer than the staleness limit")
}

func TestInvalidResourceVersionIsBadRequestWhenStale(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	f.clock.Step(stalenessLimit + time.Nanosecond)
	if _, err := f.repositories.Watch(inNamespace("ns1"), from("x")); !apierrors.IsBadRequest(err) {
		t.Errorf("watch: got %v, want BadRequest", err)
	}
	if _, err := f.repositories.List(inNamespace("ns1"), from("x")); !apierrors.IsBadRequest(err) {
		t.Errorf("list: got %v, want BadRequest", err)
	}
}

func TestWatchEndsWhenStale(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	rv := resourceVersion(t, f.repositories)
	w := startWatch(t, f.repositories, "ns1", from(rv))
	other := startWatch(t, f.repositories, "other", from(rv))
	f.harbor.repositories[0].PullCount++
	f.poll(t)
	nextEvents(t, w, 1)
	// The watcher waits for more.
	expectNoEvent(t, w)

	f.clock.Step(stalenessLimit + time.Nanosecond)
	f.harbor.err = harbor.ErrUnavailable
	f.poll(t)
	if diff := cmp.Diff([]string{"ERROR ServiceUnavailable"}, nextEvents(t, w, 1)); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
	expectEnd(t, w)
	expectNoEvent(t, other)
}

func TestWatchEndsOnceStaleDuringARead(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	w := startWatch(t, f.repositories, "ns1", from(resourceVersion(t, f.repositories)))
	stepWhenWaiting(t, f, stalenessLimit+time.Nanosecond)
	if diff := cmp.Diff([]string{"ERROR ServiceUnavailable"}, nextEvents(t, w, 1)); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
	expectEnd(t, w)
}

func TestWatchSendsNoBookmarkOnceStale(t *testing.T) {
	// The bookmark and staleness are due at once, and the watch sees them in random order.
	for range 10 {
		f := newFixture(t, repositoryHarbor())
		f.store.bookmarkInterval = stalenessLimit + time.Nanosecond
		w := startWatch(t, f.repositories, "ns1", &metainternalversion.ListOptions{ResourceVersion: resourceVersion(t, f.repositories), AllowWatchBookmarks: true})
		stepWhenWaiting(t, f, stalenessLimit+time.Nanosecond)
		if diff := cmp.Diff([]string{"ERROR ServiceUnavailable"}, nextEvents(t, w, 1)); diff != "" {
			t.Errorf("events (-want +got):\n%s", diff)
		}
		expectEnd(t, w)
	}
}

func TestWatchEndsWhenTheReadGetsOlder(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	w := startWatch(t, f.repositories, "ns1", from(resourceVersion(t, f.repositories)))
	expectNoEvent(t, w)
	f.store.update(maps.Clone(f.store.items), time.Time{}, errArtifactsRead)
	if diff := cmp.Diff([]string{"ERROR ServiceUnavailable"}, nextEvents(t, w, 1)); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
}

func TestPollsWithoutChangesWakeNoWatch(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	_, changed, _, err := f.store.changesAfter(parseRV(t, resourceVersion(t, f.repositories)), "ns1")
	if err != nil {
		t.Fatal(err)
	}
	f.poll(t)
	f.harbor.err = harbor.ErrUnavailable
	f.poll(t)
	select {
	case <-changed:
		t.Error("woke watches")
	default:
	}
}

func TestPollsWithoutChangesKeepTheItems(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	items := f.store.items
	f.poll(t)
	if reflect.ValueOf(f.store.items).UnsafePointer() != reflect.ValueOf(items).UnsafePointer() {
		t.Error("replaced the items")
	}
}

func TestStartingAWatchDoesNotCopyTheState(t *testing.T) {
	f := newUnreadFixture(&fakeHarbor{})
	items := map[key]*item{}
	for i := range 1000 {
		name := fmt.Sprintf("r%d", i)
		items[key{repositoriesResource.Resource, name}] = &item{harborID: int64(i), obj: &v1alpha1.HarborRepository{ObjectMeta: metav1.ObjectMeta{Name: name}}}
	}
	f.store.update(items, f.clock.Now(), nil)
	for i := range 100 {
		f.store.SetNamespace(fmt.Sprintf("n%d", i), true)
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range 10 {
		startWatch(t, f.repositories, "", from("0"))
	}
	runtime.ReadMemStats(&after)
	if n := after.TotalAlloc - before.TotalAlloc; n > 10<<20 {
		t.Errorf("starting 10 watches of 100000 objects allocated %d MiB", n>>20)
	}
}

func TestWatchEndsWithContext(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	for _, opts := range []*metainternalversion.ListOptions{
		from(resourceVersion(t, f.repositories)),
		{ResourceVersion: resourceVersion(t, f.repositories), AllowWatchBookmarks: true},
		// Nothing reads the initial events.
		from("0"),
	} {
		ctx, cancel := context.WithCancel(inNamespace(""))
		w, err := f.repositories.Watch(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
		cancel()
		expectClosed(t, w)
	}
}

func TestWatchEndsWhenStopped(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	for _, opts := range []*metainternalversion.ListOptions{from(resourceVersion(t, f.repositories)), from("0")} {
		w, err := f.repositories.Watch(inNamespace(""), opts)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
		go w.Stop()
		expectClosed(t, w)
	}
}

// expectClosed fails unless w ends without anything reading from it.
func expectClosed(t *testing.T, w watch.Interface) {
	t.Helper()
	select {
	case <-w.(*watcher).done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch stayed open")
	}
	if _, ok := <-w.ResultChan(); ok {
		t.Error("watch left its result channel open")
	}
}

func TestEventObjectsAreCopies(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	w := startWatch(t, f.repositories, "ns1", from("0"))
	o, err := meta.Accessor(nextEvent(t, w).Object)
	if err != nil {
		t.Fatal(err)
	}
	o.SetLabels(map[string]string{"changed": "true"})
	for _, r := range listItems(t, f.repositories, "", nil) {
		if len(r.Labels) > 0 {
			t.Errorf("%s/%s has labels %v", r.Namespace, r.Name, r.Labels)
		}
	}
}

// randomRepositories returns some of six repositories, each with one of two IDs and pull counts.
func randomRepositories(rng *rand.Rand) []harbor.Repository {
	var repos []harbor.Repository
	for i := range 6 {
		if rng.IntN(2) == 0 {
			repos = append(repos, harbor.Repository{ID: int64(10*i + rng.IntN(2)), Name: fmt.Sprintf("proj/r%d", i), PullCount: int64(rng.IntN(2))})
		}
	}
	return repos
}

// listedByName lists repositories in all namespaces, keyed by namespace and name.
func listedByName(t *testing.T, f *fixture) map[string]v1alpha1.HarborRepository {
	t.Helper()
	listed := map[string]v1alpha1.HarborRepository{}
	for _, r := range listItems(t, f.repositories, "", nil) {
		listed[r.Namespace+"/"+r.Name] = r
	}
	return listed
}

// replay applies w's events to state until it sees the object named last.
// It checks that the resourceVersions of the events after the first initial ones increase from rv.
// If marked, a bookmark at rv must mark the end of the initial events.
func replay(t *testing.T, w watch.Interface, state map[string]v1alpha1.HarborRepository, rv uint64, initial int, marked bool, last string) map[string]v1alpha1.HarborRepository {
	t.Helper()
	for i := 0; ; i++ {
		e := nextEvent(t, w)
		o := e.Object.(*v1alpha1.HarborRepository)
		if marked && i == initial {
			if e.Type != watch.Bookmark || parseRV(t, o.ResourceVersion) != rv || o.Annotations[metav1.InitialEventsAnnotationKey] != "true" {
				t.Fatalf("%s at %s ends the initial events, want a bookmark at %d", describe(e), o.ResourceVersion, rv)
			}
			continue
		}
		k := o.Namespace + "/" + o.Name
		if next := parseRV(t, o.ResourceVersion); i < initial && (e.Type != watch.Added || next > rv) {
			t.Fatalf("initial %s at %d, after %d", describe(e), next, rv)
		} else if i >= initial && next <= rv {
			t.Fatalf("%s at %d follows %d", describe(e), next, rv)
		} else if i >= initial {
			rv = next
		}
		_, exists := state[k]
		switch {
		case e.Type == watch.Added && !exists, e.Type == watch.Modified && exists:
			state[k] = *o
		case e.Type == watch.Deleted && exists:
			delete(state, k)
		default:
			t.Fatalf("%s, but it exists: %v", describe(e), exists)
		}
		if k == last {
			return state
		}
	}
}

func TestWatchesReplayToTheLatestList(t *testing.T) {
	for seed := range uint64(20) {
		rng := rand.New(rand.NewPCG(seed, seed))
		f := newFixture(t, &fakeHarbor{})
		type start struct {
			listed                      map[string]v1alpha1.HarborRepository
			rv                          uint64
			resumed, initial, watchList watch.Interface
		}
		var starts []start
		for range 30 {
			if rng.IntN(3) == 0 {
				f.store.SetNamespace(fmt.Sprintf("ns%d", rng.IntN(4)), rng.IntN(2) == 0)
			} else {
				f.harbor.repositories = randomRepositories(rng)
				f.poll(t)
			}
			rv := resourceVersion(t, f.repositories)
			starts = append(starts, start{
				listed:  listedByName(t, f),
				rv:      parseRV(t, rv),
				resumed: startWatch(t, f.repositories, "", from(rv)),
				initial: startWatch(t, f.repositories, "", from("0")),
				watchList: startWatch(t, f.repositories, "", &metainternalversion.ListOptions{
					SendInitialEvents:    ptr.To(true),
					ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
					AllowWatchBookmarks:  true,
				}),
			})
		}
		// The newest repository's event in a new namespace comes last.
		f.harbor.repositories = append(f.harbor.repositories, harbor.Repository{ID: 100, Name: "proj/last"})
		f.poll(t)
		f.store.SetNamespace("last", true)
		want := listedByName(t, f)

		for i, s := range starts {
			if diff := cmp.Diff(want, replay(t, s.initial, map[string]v1alpha1.HarborRepository{}, s.rv, len(s.listed), false, "last/last")); diff != "" {
				t.Fatalf("seed %d, watch with the state at %d (-want +got):\n%s", seed, i, diff)
			}
			if diff := cmp.Diff(want, replay(t, s.watchList, map[string]v1alpha1.HarborRepository{}, s.rv, len(s.listed), true, "last/last")); diff != "" {
				t.Fatalf("seed %d, watch list with the state at %d (-want +got):\n%s", seed, i, diff)
			}
			if diff := cmp.Diff(want, replay(t, s.resumed, s.listed, s.rv, 0, false, "last/last")); diff != "" {
				t.Fatalf("seed %d, watch with the state at %d (-want +got):\n%s", seed, i, diff)
			}
		}
	}
}
