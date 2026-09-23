package apiserver_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	restclient "k8s.io/client-go/rest"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apiserver"
)

func newHandler(t *testing.T) http.Handler {
	t.Helper()
	c := apiserver.NewConfig()
	c.ExternalAddress = "localhost:443"
	c.LoopbackClientConfig = &restclient.Config{}
	s, err := apiserver.New(c.Complete(nil))
	if err != nil {
		t.Fatal(err)
	}
	return s.PrepareRun().Handler
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestDiscovery(t *testing.T) {
	rec := get(t, newHandler(t), "/apis/harbor.goharbor.io/v1alpha1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var list metav1.APIResourceList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"harborrepositories": "HarborRepository",
		"harborartifacts":    "HarborArtifact",
	}
	if len(list.APIResources) != len(want) {
		t.Fatalf("got %d resources, want %d: %+v", len(list.APIResources), len(want), list.APIResources)
	}
	for _, r := range list.APIResources {
		if want[r.Name] != r.Kind {
			t.Errorf("resource %q has kind %q, want %q", r.Name, r.Kind, want[r.Name])
		}
		if !r.Namespaced {
			t.Errorf("resource %q is not namespaced", r.Name)
		}
		if r.SingularName != strings.ToLower(r.Kind) {
			t.Errorf("resource %q has singular name %q", r.Name, r.SingularName)
		}
		verbs := slices.Sorted(slices.Values(r.Verbs))
		if !slices.Equal(verbs, []string{"get", "list"}) {
			t.Errorf("resource %q has verbs %v, want [get list]", r.Name, verbs)
		}
	}
}

func TestListIsEmpty(t *testing.T) {
	h := newHandler(t)
	for _, tc := range []struct{ path, kind string }{
		{"/apis/harbor.goharbor.io/v1alpha1/namespaces/default/harborrepositories", "HarborRepositoryList"},
		{"/apis/harbor.goharbor.io/v1alpha1/harborartifacts", "HarborArtifactList"},
	} {
		rec := get(t, h, tc.path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d: %s", tc.path, rec.Code, rec.Body)
		}
		var list struct {
			Kind  string
			Items []json.RawMessage
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		if list.Kind != tc.kind || len(list.Items) != 0 {
			t.Errorf("GET %s: got kind %q with %d items", tc.path, list.Kind, len(list.Items))
		}
	}
}

func TestGetIsNotFound(t *testing.T) {
	rec := get(t, newHandler(t), "/apis/harbor.goharbor.io/v1alpha1/namespaces/default/harborrepositories/nginx")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var status metav1.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Reason != metav1.StatusReasonNotFound {
		t.Errorf("reason %q", status.Reason)
	}
}

func TestOpenAPIDescribesKinds(t *testing.T) {
	rec := get(t, newHandler(t), "/openapi/v3/apis/harbor.goharbor.io/v1alpha1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	for _, def := range []string{"HarborRepository", "HarborArtifact"} {
		if !strings.Contains(rec.Body.String(), `"io.goharbor.harbor.v1alpha1.`+def+`"`) {
			t.Errorf("OpenAPI v3 lacks %s", def)
		}
	}
}
