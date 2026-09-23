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

type fakeHarbor struct{}

func (fakeHarbor) ListRepositories(context.Context, string) ([]harbor.Repository, error) {
	return []harbor.Repository{{ID: 1, Name: "proj/app"}}, nil
}

func (fakeHarbor) GetRepository(context.Context, string, string) (*harbor.Repository, error) {
	return nil, harbor.ErrNotFound
}

func (fakeHarbor) ListArtifacts(context.Context, string, string, string) ([]harbor.Artifact, error) {
	return nil, nil
}

func namespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// startServer runs the post-start hooks of a server for project "proj" and returns its handler.
func startServer(t *testing.T, kube *fake.Clientset) http.Handler {
	t.Helper()
	c := apiserver.NewConfig()
	c.ExternalAddress = "localhost:443"
	c.LoopbackClientConfig = &restclient.Config{}
	s, err := newServer(c.Complete(nil), kube, fakeHarbor{}, "proj")
	if err != nil {
		t.Fatal(err)
	}
	h := s.PrepareRun().Handler
	s.RunPostStartHooks(t.Context())
	return h
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
	h := startServer(t, kube)

	if code, body := serve(h, "/readyz?verbose"); code != http.StatusInternalServerError || !strings.Contains(body, "[-]namespaces-synced failed") {
		t.Errorf("before listing namespaces: /readyz returned %d:\n%s", code, body)
	}
	waitForOK(t, h, "/livez?verbose")

	close(releaseList)
	waitForOK(t, h, "/readyz?verbose")
}

func TestServerWatchesOnlyLabeledNamespaces(t *testing.T) {
	kube := fake.NewClientset(
		namespace("labeled", map[string]string{namespaces.ProjectLabel: "proj"}),
		namespace("other-project", map[string]string{namespaces.ProjectLabel: "other"}),
		namespace("unlabeled", nil),
	)
	h := startServer(t, kube)
	waitForOK(t, h, "/readyz")

	code, body := serve(h, "/apis/harbor.goharbor.io/v1alpha1/harborrepositories")
	var list v1alpha1.HarborRepositoryList
	if err := json.Unmarshal([]byte(body), &list); code != http.StatusOK || err != nil {
		t.Fatalf("list returned %d, %v: %s", code, err, body)
	}
	var visible []string
	for _, r := range list.Items {
		visible = append(visible, r.Namespace+"/"+r.Name)
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
