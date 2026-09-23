package registry

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

func TestRequestsSeeTheLastPoll(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	f.harbor.repositories = []harbor.Repository{
		{ID: 1, Name: "proj/nginx", PullCount: 121},
		{ID: 4, Name: "proj/added"},
	}
	f.poll(t)

	items := listItems(t, f.repositories, "ns1", nil)
	if diff := cmp.Diff([]string{"ns1/added", "ns1/nginx"}, names(items)); diff != "" {
		t.Errorf("names (-want +got):\n%s", diff)
	}
	if pulls := items[1].Status.PullCount; pulls != 121 {
		t.Errorf("nginx has %d pulls, want 121", pulls)
	}
	if _, err := f.repositories.Get(inNamespace("ns1"), "team.api", &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("get deleted: got %v, want NotFound", err)
	}
}

func TestRequestsIgnoreResourceVersion(t *testing.T) {
	f := newFixture(t, repositoryHarbor())
	want := listItems(t, f.repositories, "ns1", nil)
	for _, rv := range []string{"0", "1", "18446744073709551615", "x"} {
		for _, match := range []metav1.ResourceVersionMatch{"", metav1.ResourceVersionMatchExact, metav1.ResourceVersionMatchNotOlderThan} {
			obj, err := f.repositories.List(inNamespace("ns1"), &metainternalversion.ListOptions{ResourceVersion: rv, ResourceVersionMatch: match})
			if err != nil {
				t.Errorf("list at resourceVersion %q, match %q: %v", rv, match, err)
				continue
			}
			list := obj.(*v1alpha1.HarborRepositoryList)
			if diff := cmp.Diff(want, list.Items); diff != "" || list.ResourceVersion != "" {
				t.Errorf("list at resourceVersion %q, match %q: resourceVersion %q, items (-want +got):\n%s", rv, match, list.ResourceVersion, diff)
			}
		}
		obj, err := f.repositories.Get(inNamespace("ns1"), "nginx", &metav1.GetOptions{ResourceVersion: rv})
		if err != nil {
			t.Errorf("get at resourceVersion %q: %v", rv, err)
		} else if diff := cmp.Diff(&want[1], obj); diff != "" {
			t.Errorf("get at resourceVersion %q (-want +got):\n%s", rv, diff)
		}
	}
}

func TestObjectsAreCopies(t *testing.T) {
	a, _ := newArtifacts(t)
	obj, err := a.Get(inNamespace("ns1"), "team.api.sha256-aaaaaaaaaaaa", &metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	obj.(*v1alpha1.HarborArtifact).Labels[v1alpha1.RepositoryLabel] = "changed"
	for _, item := range artifactItems(t, a, "", nil) {
		if label := item.Labels[v1alpha1.RepositoryLabel]; label == "changed" {
			t.Errorf("%s/%s has repository label %q", item.Namespace, item.Name, label)
		}
	}
}

// requests returns the errors of a get and a list of repositories in namespace.
func requests(f *fixture, namespace string) []error {
	_, getErr := f.repositories.Get(inNamespace(namespace), "nginx", &metav1.GetOptions{})
	_, listErr := f.repositories.List(inNamespace(namespace), nil)
	return []error{getErr, listErr}
}

func expectUnavailable(t *testing.T, f *fixture, namespace, message string) {
	t.Helper()
	for i, err := range requests(f, namespace) {
		if !apierrors.IsServiceUnavailable(err) || !strings.Contains(err.Error(), message) {
			t.Errorf("%s in %q: got %v, want ServiceUnavailable: %s", []string{"get", "list"}[i], namespace, err, message)
		}
	}
}

func expectAvailable(t *testing.T, f *fixture, namespace string) {
	t.Helper()
	for i, err := range requests(f, namespace) {
		if err != nil {
			t.Errorf("%s in %q: %v", []string{"get", "list"}[i], namespace, err)
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
