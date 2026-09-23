package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	restclient "k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apiserver"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/namespaces"
)

// fakeHarbor serves repository app, or fails with err. Its lists wait until release is closed.
type fakeHarbor struct {
	err     error
	release chan struct{}
}

func (h fakeHarbor) ListRepositories(ctx context.Context, _ string) ([]harbor.Repository, error) {
	if h.release != nil {
		select {
		case <-h.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []harbor.Repository{{ID: 1, Name: "proj/app"}}, h.err
}

func (h fakeHarbor) ListArtifacts(context.Context, string, string) ([]harbor.Artifact, error) {
	return nil, h.err
}

func namespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

var testHarborOptions = harborOptions{Project: "proj", PollInterval: 10 * time.Millisecond, StalenessLimit: time.Minute}

// startServer runs the post-start hooks of a server for project "proj" and returns its handler.
func startServer(t *testing.T, kube *fake.Clientset, h fakeHarbor) http.Handler {
	t.Helper()
	return startServerWithOptions(t, kube, h, testHarborOptions)
}

func startServerWithOptions(t *testing.T, kube *fake.Clientset, h fakeHarbor, o harborOptions) http.Handler {
	t.Helper()
	c := apiserver.NewConfig()
	c.ExternalAddress = "localhost:443"
	c.LoopbackClientConfig = &restclient.Config{}
	s, err := newServer(c.Complete(nil), kube, h, &o)
	if err != nil {
		t.Fatal(err)
	}
	handler := s.PrepareRun().Handler
	s.RunPostStartHooks(t.Context())
	return handler
}

func serve(h http.Handler, path string) (int, string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func waitForOK(t *testing.T, h http.Handler, path string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		code, body := serve(h, path)
		if code == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s returned %d:\n%s", path, code, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServerIsReadyOnceNamespacesSync(t *testing.T) {
	kube := fake.NewClientset()
	releaseList := make(chan struct{})
	kube.PrependReactor("list", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
		<-releaseList
		return false, nil, nil
	})
	h := startServer(t, kube, fakeHarbor{})

	if code, body := serve(h, "/readyz?verbose"); code != http.StatusInternalServerError || !strings.Contains(body, "[-]namespaces-synced failed") {
		t.Errorf("before listing namespaces: /readyz returned %d:\n%s", code, body)
	}
	waitForOK(t, h, "/livez?verbose")

	close(releaseList)
	waitForOK(t, h, "/readyz?verbose")
}

func TestServerIsReadyOnceItHasTriedToReadHarbor(t *testing.T) {
	kube := fake.NewClientset(namespace("labeled", map[string]string{namespaces.ProjectLabel: "proj"}))
	release := make(chan struct{})
	o := testHarborOptions
	o.Timeout = time.Minute
	h := startServerWithOptions(t, kube, fakeHarbor{release: release}, o)

	time.Sleep(50 * time.Millisecond)
	if code, body := serve(h, "/readyz?verbose"); code != http.StatusInternalServerError || !strings.Contains(body, "[-]harbor-read failed") {
		t.Errorf("before reading Harbor: /readyz returned %d:\n%s", code, body)
	}
	close(release)
	waitForOK(t, h, "/readyz?verbose")
}

func TestServerIsReadyWithinTwiceTheHarborTimeout(t *testing.T) {
	kube := fake.NewClientset()
	o := testHarborOptions
	o.Timeout = 50 * time.Millisecond
	h := startServerWithOptions(t, kube, fakeHarbor{release: make(chan struct{})}, o)
	waitForOK(t, h, "/readyz?verbose")
}

func TestReadinessDoesNotDependOnHarbor(t *testing.T) {
	kube := fake.NewClientset(namespace("labeled", map[string]string{namespaces.ProjectLabel: "proj"}))
	h := startServer(t, kube, fakeHarbor{err: harbor.ErrUnavailable})
	waitForOK(t, h, "/readyz")
	if code, body := serve(h, "/apis/harbor.goharbor.io/v1alpha1/namespaces/labeled/harborrepositories"); code != http.StatusServiceUnavailable {
		t.Errorf("list returned %d: %s", code, body)
	}
}

func TestServerWatchesOnlyLabeledNamespaces(t *testing.T) {
	kube := fake.NewClientset(
		namespace("labeled", map[string]string{namespaces.ProjectLabel: "proj"}),
		namespace("other-project", map[string]string{namespaces.ProjectLabel: "other"}),
		namespace("unlabeled", nil),
	)
	h := startServer(t, kube, fakeHarbor{})
	waitForOK(t, h, "/readyz")

	var visible []string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		code, body := serve(h, "/apis/harbor.goharbor.io/v1alpha1/harborrepositories")
		var list v1alpha1.HarborRepositoryList
		if err := json.Unmarshal([]byte(body), &list); code == http.StatusOK && err == nil {
			visible = nil
			for _, r := range list.Items {
				visible = append(visible, r.Namespace+"/"+r.Name)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("list returned %d: %s", code, body)
		}
	}
	if diff := cmp.Diff([]string{"labeled/app"}, visible); diff != "" {
		t.Errorf("repositories (-want +got):\n%s", diff)
	}

	for _, a := range kube.Actions() {
		if l, ok := a.(clienttesting.ListAction); ok && l.GetListRestrictions().Labels.String() != namespaces.ProjectLabel {
			t.Errorf("listed namespaces matching %q, want only those with the %s label", l.GetListRestrictions().Labels, namespaces.ProjectLabel)
		}
		if w, ok := a.(clienttesting.WatchAction); ok && w.GetWatchRestrictions().Labels.String() != namespaces.ProjectLabel {
			t.Errorf("watched namespaces matching %q, want only those with the %s label", w.GetWatchRestrictions().Labels, namespaces.ProjectLabel)
		}
	}
}
