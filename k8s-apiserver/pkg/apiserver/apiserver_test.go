package apiserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authorization/authorizerfactory"
	restclient "k8s.io/client-go/rest"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apiserver"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/registry"
)

var kinds = []struct{ resource, kind string }{
	{"harborrepositories", "HarborRepository"},
	{"harborartifacts", "HarborArtifact"},
}

type fakeHarbor struct{}

func (fakeHarbor) ListRepositories(context.Context, string) ([]harbor.Repository, error) {
	return []harbor.Repository{{ID: 1, Name: "proj/team/api", ArtifactCount: 2, PullCount: 3}}, nil
}

func (fakeHarbor) ListArtifacts(_ context.Context, _, repository string) ([]harbor.Artifact, error) {
	if repository != "team/api" {
		return nil, harbor.ErrNotFound
	}
	return []harbor.Artifact{{ID: 1, Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Type: "IMAGE"}}, nil
}

// allowed lets only the "allowed" namespace see the project.
type allowed struct{}

func (allowed) Allows(ns string) bool { return ns == "allowed" }
func (allowed) Namespaces() []string  { return []string{"allowed"} }
func (allowed) Namespace(ns string) (*corev1.Namespace, bool) {
	if ns != "allowed" {
		return nil, false
	}
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: "allowed-uid"}}, true
}

func newHandler(t *testing.T) http.Handler {
	t.Helper()
	return newHandlerWithReplications(t, nil)
}

func newHandlerWithReplications(t *testing.T, replications *registry.Replications) http.Handler {
	t.Helper()
	store := registry.NewStore("proj", time.Minute, allowed{})
	if err := registry.NewPoller(fakeHarbor{}, store).Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	c := apiserver.NewConfig()
	c.ExternalAddress = "localhost:443"
	c.LoopbackClientConfig = &restclient.Config{}
	// Server-side apply authorizes the create that it may do.
	c.Authorization.Authorizer = authorizerfactory.NewAlwaysAllowAuthorizer()
	s, err := apiserver.New(c.Complete(nil), store, replications)
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

func TestListIsEmptyOutsideAllowedNamespaces(t *testing.T) {
	h := newHandler(t)
	for _, k := range kinds {
		var list struct {
			Kind  string
			Items []json.RawMessage
		}
		decode(t, get(t, h, "/apis/harbor.goharbor.io/v1alpha1/namespaces/default/"+k.resource), http.StatusOK, &list)
		if list.Kind != k.kind+"List" || len(list.Items) != 0 {
			t.Errorf("%s: got kind %q with %d items", k.resource, list.Kind, len(list.Items))
		}
	}
}

func TestWatchIsNotSupported(t *testing.T) {
	h := newHandler(t)
	for _, k := range kinds {
		for _, path := range []string{"/apis/harbor.goharbor.io/v1alpha1/namespaces/allowed/", "/apis/harbor.goharbor.io/v1alpha1/"} {
			var status metav1.Status
			decode(t, get(t, h, path+k.resource+"?watch=true"), http.StatusMethodNotAllowed, &status)
			if status.Reason != metav1.StatusReasonMethodNotAllowed {
				t.Errorf("%s: reason %q", path+k.resource, status.Reason)
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

func TestRepositoryTable(t *testing.T) {
	var table metav1.Table
	decode(t, get(t, newHandler(t), "/apis/harbor.goharbor.io/v1alpha1/namespaces/allowed/harborrepositories",
		"application/json;as=Table;v=v1;g=meta.k8s.io"), http.StatusOK, &table)
	if len(table.Rows) != 1 || table.Rows[0].Cells[0] != "team.api" {
		t.Errorf("rows %+v", table.Rows)
	}
}

func TestArtifactTable(t *testing.T) {
	h := newHandler(t)
	var table metav1.Table
	decode(t, get(t, h, "/apis/harbor.goharbor.io/v1alpha1/namespaces/allowed/harborartifacts",
		"application/json;as=Table;v=v1;g=meta.k8s.io"), http.StatusOK, &table)
	if len(table.Rows) != 1 || table.Rows[0].Cells[0] != "team.api.sha256-0123456789ab" {
		t.Errorf("rows %+v", table.Rows)
	}

	rec := get(t, h, "/apis/harbor.goharbor.io/v1alpha1/namespaces/default/harborartifacts",
		"application/json;as=Table;v=v1;g=meta.k8s.io")
	if !strings.Contains(rec.Body.String(), `"rows":[]`) {
		t.Errorf("empty table: %s", rec.Body)
	}
}

func TestFieldSelectors(t *testing.T) {
	h := newHandler(t)
	for _, tc := range []struct {
		resource, selector  string
		wantCode, wantItems int
	}{
		{"harborrepositories", "metadata.name=team.api", http.StatusOK, 1},
		{"harborrepositories", "metadata.namespace=allowed", http.StatusOK, 1},
		{"harborrepositories", "status.name=proj%2Fteam%2Fapi", http.StatusBadRequest, 0},
		{"harborartifacts", "metadata.name=team.api.sha256-0123456789ab", http.StatusOK, 1},
		{"harborartifacts", "metadata.namespace=allowed", http.StatusOK, 1},
		{"harborartifacts", "status.repository=proj%2Fteam%2Fapi", http.StatusOK, 1},
		{"harborartifacts", "status.repository=proj%2Fother", http.StatusOK, 0},
		{"harborartifacts", "status.digest=x", http.StatusBadRequest, 0},
	} {
		rec := get(t, h, "/apis/harbor.goharbor.io/v1alpha1/"+tc.resource+"?fieldSelector="+tc.selector)
		if rec.Code != tc.wantCode {
			t.Errorf("%s %s: status %d, want %d: %s", tc.resource, tc.selector, rec.Code, tc.wantCode, rec.Body)
			continue
		}
		var list struct{ Items []json.RawMessage }
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		if tc.wantCode == http.StatusOK && len(list.Items) != tc.wantItems {
			t.Errorf("%s %s: %d items, want %d", tc.resource, tc.selector, len(list.Items), tc.wantItems)
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
	h := newReplicationHandler(t)
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
		for _, k := range append(kinds, struct{ resource, kind string }{"harborreplications", "HarborReplication"}) {
			for _, kind := range []string{k.kind, k.kind + "List"} {
				def, _ := defs["io.goharbor.harbor.v1alpha1."+kind].(map[string]any)
				gvks, _ := json.Marshal(def["x-kubernetes-group-version-kind"])
				want := `[{"group":"harbor.goharbor.io","kind":"` + kind + `","version":"v1alpha1"}]`
				if string(gvks) != want {
					t.Errorf("%s: %s has GVKs %s, want %s", tc.path, kind, gvks, want)
				}
			}
		}
	}
}

// fakeReplicationHarbor has registry docker-hub, and keeps the policies that it creates. They never run.
type fakeReplicationHarbor struct {
	registry.ReplicationHarbor
	policies []harbor.ReplicationPolicy
}

func (*fakeReplicationHarbor) ListRegistries(context.Context) ([]harbor.Registry, error) {
	return []harbor.Registry{{ID: 1, Name: "docker-hub"}}, nil
}

func (f *fakeReplicationHarbor) ListReplicationPolicies(_ context.Context, prefix string) ([]harbor.ReplicationPolicy, error) {
	return slices.DeleteFunc(slices.Clone(f.policies), func(p harbor.ReplicationPolicy) bool { return !strings.HasPrefix(p.Name, prefix) }), nil
}

func (f *fakeReplicationHarbor) CreateReplicationPolicy(_ context.Context, p *harbor.ReplicationPolicy) (int64, error) {
	stored := *p
	stored.ID = int64(len(f.policies) + 1)
	stored.SrcRegistry = &harbor.Registry{ID: 1, Name: "docker-hub"}
	f.policies = append(f.policies, stored)
	return stored.ID, nil
}

func (f *fakeReplicationHarbor) GetReplicationPolicy(_ context.Context, id int64) (*harbor.ReplicationPolicy, error) {
	return &f.policies[id-1], nil
}

func (*fakeReplicationHarbor) StartReplication(context.Context, int64) (int64, error) { return 1, nil }

func (*fakeReplicationHarbor) LatestReplicationExecution(context.Context, int64) (*harbor.ReplicationExecution, error) {
	return nil, nil
}

func discover(t *testing.T, h http.Handler, resource string) (metav1.APIResource, bool) {
	t.Helper()
	var list metav1.APIResourceList
	decode(t, get(t, h, "/apis/harbor.goharbor.io/v1alpha1"), http.StatusOK, &list)
	i := slices.IndexFunc(list.APIResources, func(r metav1.APIResource) bool { return r.Name == resource })
	if i < 0 {
		return metav1.APIResource{}, false
	}
	return list.APIResources[i], true
}

func TestReplicationsAreAbsentWhenDisabled(t *testing.T) {
	h := newHandler(t)
	if r, ok := discover(t, h, "harborreplications"); ok {
		t.Errorf("discovered %+v", r)
	}
	if rec := get(t, h, "/apis/harbor.goharbor.io/v1alpha1/namespaces/allowed/harborreplications"); rec.Code != http.StatusNotFound {
		t.Errorf("list returned %d: %s", rec.Code, rec.Body)
	}
}

func newReplicationHandler(t *testing.T) http.Handler {
	t.Helper()
	return newHandlerWithReplications(t, registry.NewReplications(&fakeReplicationHarbor{}, allowed{},
		registry.ReplicationConfig{Project: "proj", Prefix: "k8s", Registries: []string{"docker-hub"}}))
}

func send(t *testing.T, h http.Handler, method, path, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestReplicationsArePresentWhenEnabled(t *testing.T) {
	h := newReplicationHandler(t)

	r, ok := discover(t, h, "harborreplications")
	if !ok {
		t.Fatal("harborreplications missing")
	}
	if r.Kind != "HarborReplication" || !r.Namespaced || r.SingularName != "harborreplication" {
		t.Errorf("kind %q, namespaced %v, singular %q", r.Kind, r.Namespaced, r.SingularName)
	}
	want := []string{"create", "delete", "get", "list", "patch", "update"}
	if verbs := slices.Sorted(slices.Values(r.Verbs)); !slices.Equal(verbs, want) {
		t.Errorf("verbs %v, want %v", verbs, want)
	}

	body := `{"apiVersion":"harbor.goharbor.io/v1alpha1","kind":"HarborReplication","metadata":{"name":"nginx"},` +
		`"spec":{"registry":"docker-hub","repository":"library/nginx","tag":"1.27"}}`
	rec := send(t, h, http.MethodPost, "/apis/harbor.goharbor.io/v1alpha1/namespaces/allowed/harborreplications?dryRun=All", "application/json", body)
	checkCreated(t, rec)
}

func checkCreated(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var created struct {
		Kind   string
		Status struct{ Destination string }
	}
	decode(t, rec, http.StatusCreated, &created)
	if created.Kind != "HarborReplication" || created.Status.Destination != "proj/k8s/allowed/nginx" {
		t.Errorf("created %+v", created)
	}
}

func TestServerSideApplyCreatesReplication(t *testing.T) {
	body := `
apiVersion: harbor.goharbor.io/v1alpha1
kind: HarborReplication
metadata:
  name: nginx
spec:
  registry: docker-hub
  repository: library/nginx
  tag: "1.27"
`
	rec := send(t, newReplicationHandler(t), http.MethodPatch,
		"/apis/harbor.goharbor.io/v1alpha1/namespaces/allowed/harborreplications/nginx?dryRun=All&fieldManager=test", "application/apply-patch+yaml", body)
	checkCreated(t, rec)
}

// kubectl validates a kind on the server only if the kind's PATCH operation takes fieldValidation.
// Otherwise it lists CRDs, which the view, edit, and admin roles do not allow.
func TestReplicationPatchTakesFieldValidation(t *testing.T) {
	var doc struct {
		Paths map[string]struct {
			Patch *struct {
				GVK        map[string]string `json:"x-kubernetes-group-version-kind"`
				Parameters []struct{ Name, In string }
			}
		}
	}
	decode(t, get(t, newReplicationHandler(t), "/openapi/v3/apis/harbor.goharbor.io/v1alpha1"), http.StatusOK, &doc)
	for _, item := range doc.Paths {
		if item.Patch == nil || item.Patch.GVK["kind"] != "HarborReplication" {
			continue
		}
		for _, p := range item.Patch.Parameters {
			if p.Name == "fieldValidation" && p.In == "query" {
				return
			}
		}
	}
	t.Error("no PATCH operation of HarborReplication takes the query parameter fieldValidation")
}

func TestReplicationUpdatesAcceptOnlyUnchangedReplications(t *testing.T) {
	h := newReplicationHandler(t)
	path := "/apis/harbor.goharbor.io/v1alpha1/namespaces/allowed/harborreplications/"
	const apply, manifest = "application/apply-patch+yaml", `
apiVersion: harbor.goharbor.io/v1alpha1
kind: HarborReplication
metadata:
  name: nginx
  labels:
    app: web
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: "{}"
spec:
  registry: docker-hub
  repository: library/nginx
  tag: "1.27"
`
	newTag := strings.Replace(manifest, "1.27", "1.28", 1)
	withoutLabels := strings.Replace(manifest, "  labels:\n    app: web\n", "", 1)
	withoutTag := strings.Replace(manifest, "  tag: \"1.27\"\n", "", 1)
	withoutLastApplied := strings.Replace(manifest, "  annotations:\n    kubectl.kubernetes.io/last-applied-configuration: \"{}\"\n", "", 1)
	// kubectl's server-side apply sends this to keep the annotation of an earlier client-side apply.
	onlyLastApplied := `
apiVersion: harbor.goharbor.io/v1alpha1
kind: HarborReplication
metadata:
  name: nginx
  namespace: allowed
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: "{}"
`
	onlyLabels := `
apiVersion: harbor.goharbor.io/v1alpha1
kind: HarborReplication
metadata:
  name: nginx
  labels:
    app: web
`
	checkCreated(t, send(t, h, http.MethodPatch, path+"nginx?fieldManager=test", apply, manifest))
	current := get(t, h, path+"nginx").Body.String()
	var created metav1.PartialObjectMetadata
	if err := json.Unmarshal([]byte(current), &created); err != nil {
		t.Fatal(err)
	}
	withUID := strings.Replace(manifest, "  name: nginx\n", "  name: nginx\n  uid: "+string(created.UID)+"\n", 1)
	withUIDWithoutLabels := strings.Replace(withUID, "  labels:\n    app: web\n", "", 1)

	for _, tc := range []struct {
		name, method, nameAndQuery, contentType, body string
		code                                          int
		field                                         string
	}{
		{"server-side apply", http.MethodPatch, "nginx?fieldManager=test", apply, manifest, http.StatusOK, ""},
		{"server-side apply without the labels", http.MethodPatch, "nginx?fieldManager=test", apply, withoutLabels, http.StatusUnprocessableEntity, "metadata.labels"},
		{"server-side apply without the tag", http.MethodPatch, "nginx?fieldManager=test", apply, withoutTag, http.StatusUnprocessableEntity, "spec"},
		{"server-side apply with the UID", http.MethodPatch, "nginx?fieldManager=test", apply, withUID, http.StatusOK, ""},
		{"server-side apply with the UID and without the labels", http.MethodPatch, "nginx?fieldManager=test", apply, withUIDWithoutLabels, http.StatusUnprocessableEntity, "metadata.labels"},
		// kubectl's server-side apply rewrites that annotation.
		{"server-side apply by kubectl without kubectl's last-applied configuration", http.MethodPatch, "nginx?fieldManager=kubectl", apply, withoutLastApplied, http.StatusOK, ""},
		{"server-side apply of only kubectl's last-applied configuration", http.MethodPatch, "nginx?fieldManager=kubectl-last-applied", apply, onlyLastApplied, http.StatusOK, ""},
		{"server-side apply of only the labels", http.MethodPatch, "nginx?fieldManager=test", apply, onlyLabels, http.StatusUnprocessableEntity, "spec"},
		{"merge patch of kubectl's last-applied configuration", http.MethodPatch, "nginx", "application/merge-patch+json", `{"metadata":{"annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"a\":1}"}}}`, http.StatusOK, ""},
		{"JSON patch that removes kubectl's last-applied configuration", http.MethodPatch, "nginx", "application/json-patch+json", `[{"op":"remove","path":"/metadata/annotations/kubectl.kubernetes.io~1last-applied-configuration"}]`, http.StatusOK, ""},
		{"server-side apply of a new tag", http.MethodPatch, "nginx?fieldManager=test", apply, newTag, http.StatusConflict, ""},
		{"forced server-side apply of a new tag", http.MethodPatch, "nginx?fieldManager=test&force=true", apply, newTag, http.StatusUnprocessableEntity, "spec"},
		{"update", http.MethodPut, "nginx", "application/json", current, http.StatusOK, ""},
		{"update of a missing replication", http.MethodPut, "other", "application/json", strings.Replace(current, `"name":"nginx"`, `"name":"other"`, 1), http.StatusNotFound, ""},
		{"merge patch", http.MethodPatch, "nginx", "application/merge-patch+json", `{"metadata":{"labels":{"team":"a"}}}`, http.StatusUnprocessableEntity, "metadata.labels"},
		{"strategic merge patch", http.MethodPatch, "nginx", "application/strategic-merge-patch+json", `{"spec":{"tag":"1.28"}}`, http.StatusUnprocessableEntity, "spec"},
		{"JSON patch", http.MethodPatch, "nginx", "application/json-patch+json", `[{"op":"add","path":"/metadata/annotations","value":{"a":"b"}}]`, http.StatusUnprocessableEntity, "metadata.annotations"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := send(t, h, tc.method, path+tc.nameAndQuery, tc.contentType, tc.body)
			if rec.Code != tc.code {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.code, rec.Body)
			}
			if tc.code == http.StatusOK && rec.Body.String() != current {
				t.Errorf("got %s\nwant %s", rec.Body, current)
			}
			if tc.field != "" {
				var status metav1.Status
				decode(t, rec, tc.code, &status)
				if c := status.Details.Causes; len(c) != 1 || c[0].Field != tc.field {
					t.Errorf("causes %+v, want one for %s", c, tc.field)
				}
			}
		})
	}
}
