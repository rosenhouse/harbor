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

var kinds = []struct{ resource, kind string }{
	{"harborrepositories", "HarborRepository"},
	{"harborartifacts", "HarborArtifact"},
}

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

func get(t *testing.T, h http.Handler, path string, accept ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if len(accept) > 0 {
		req.Header.Set("Accept", strings.Join(accept, ","))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, wantCode int, into any) {
	t.Helper()
	if rec.Code != wantCode {
		t.Fatalf("status %d, want %d: %s", rec.Code, wantCode, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatal(err)
	}
}

func TestDiscovery(t *testing.T) {
	var list metav1.APIResourceList
	decode(t, get(t, newHandler(t), "/apis/harbor.goharbor.io/v1alpha1"), http.StatusOK, &list)

	if len(list.APIResources) != len(kinds) {
		t.Fatalf("got %d resources, want %d: %+v", len(list.APIResources), len(kinds), list.APIResources)
	}
	for _, k := range kinds {
		i := slices.IndexFunc(list.APIResources, func(r metav1.APIResource) bool { return r.Name == k.resource })
		if i < 0 {
			t.Errorf("resource %q missing", k.resource)
			continue
		}
		r := list.APIResources[i]
		if r.Kind != k.kind || !r.Namespaced || r.SingularName != strings.ToLower(k.kind) {
			t.Errorf("resource %q: kind %q, namespaced %v, singular %q", r.Name, r.Kind, r.Namespaced, r.SingularName)
		}
		if verbs := slices.Sorted(slices.Values(r.Verbs)); !slices.Equal(verbs, []string{"get", "list"}) {
			t.Errorf("resource %q has verbs %v, want [get list]", r.Name, verbs)
		}
	}
}

func TestListIsEmpty(t *testing.T) {
	h := newHandler(t)
	for _, k := range kinds {
		for _, path := range []string{
			"/apis/harbor.goharbor.io/v1alpha1/namespaces/default/" + k.resource,
			"/apis/harbor.goharbor.io/v1alpha1/" + k.resource,
		} {
			var list struct {
				Kind  string
				Items []json.RawMessage
			}
			decode(t, get(t, h, path), http.StatusOK, &list)
			if list.Kind != k.kind+"List" || len(list.Items) != 0 {
				t.Errorf("GET %s: got kind %q with %d items", path, list.Kind, len(list.Items))
			}
		}
	}
}

func TestGetIsNotFound(t *testing.T) {
	h := newHandler(t)
	for _, k := range kinds {
		var status metav1.Status
		decode(t, get(t, h, "/apis/harbor.goharbor.io/v1alpha1/namespaces/default/"+k.resource+"/x"), http.StatusNotFound, &status)
		if status.Reason != metav1.StatusReasonNotFound || status.Details.Group != "harbor.goharbor.io" || status.Details.Kind != k.resource {
			t.Errorf("%s: reason %q, details %+v", k.resource, status.Reason, status.Details)
		}
	}
}

func TestProtobufFallsBackToJSON(t *testing.T) {
	rec := get(t, newHandler(t), "/apis/harbor.goharbor.io/v1alpha1/harborrepositories",
		"application/vnd.kubernetes.protobuf", "application/json")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestOpenAPI(t *testing.T) {
	h := newHandler(t)
	for _, tc := range []struct{ path, definitions string }{
		{"/openapi/v2", "definitions"},
		{"/openapi/v3/apis/harbor.goharbor.io/v1alpha1", "components.schemas"},
	} {
		var doc map[string]any
		decode(t, get(t, h, tc.path), http.StatusOK, &doc)
		defs := doc
		for _, key := range strings.Split(tc.definitions, ".") {
			defs, _ = defs[key].(map[string]any)
		}
		for _, k := range kinds {
			def, _ := defs["io.goharbor.harbor.v1alpha1."+k.kind].(map[string]any)
			gvks, _ := json.Marshal(def["x-kubernetes-group-version-kind"])
			want := `[{"group":"harbor.goharbor.io","kind":"` + k.kind + `","version":"v1alpha1"}]`
			if string(gvks) != want {
				t.Errorf("%s: %s has GVKs %s, want %s", tc.path, k.kind, gvks, want)
			}
		}
	}
}
