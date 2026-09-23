package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// fakePageSize is smaller than the page size that Admin asks for, so tests cover paging.
const fakePageSize = 2

// fakeHarbor serves an in-memory registry and the parts of Harbor's API that seeding uses.
// It records each request's method and escaped path.
type fakeHarbor struct {
	*httptest.Server
	mu            sync.Mutex
	requests      []string
	repositories  []string
	projects      []string
	policies      []fakePolicy
	executions    []fakeExecution
	registries    map[string][]byte
	robotRequests map[string][]byte
	stalled       string
}

type fakePolicy struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type fakeExecution struct {
	ID       int64  `json:"id"`
	PolicyID int64  `json:"policy_id"`
	Status   string `json:"status"`
}

// fuzzyMatch returns the value that a query like Harbor's q=name=~<value> asks names to contain.
func fuzzyMatch(r *http.Request) string {
	// Like Harbor, unescape q again after parsing the query string.
	q, _ := url.QueryUnescape(r.URL.Query().Get("q"))
	value, _ := strings.CutPrefix(q, "name=~")
	return value
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func newFakeHarbor(t *testing.T, repositories ...string) *fakeHarbor {
	h := &fakeHarbor{repositories: repositories, registries: map[string][]byte{}, robotRequests: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.Handle("/v2/", registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	mux.HandleFunc("POST /api/v2.0/projects", func(w http.ResponseWriter, r *http.Request) {
		var project struct {
			Name string `json:"project_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&project)
		h.mu.Lock()
		h.projects = append(h.projects, project.Name)
		h.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("GET /api/v2.0/projects/e2e", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"project_id": 3, "name": "e2e"}`)
	})
	mux.HandleFunc("GET /api/v2.0/projects/{project}/repositories", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		repos := []map[string]string{}
		for _, repo := range h.repositories {
			if strings.HasPrefix(repo, r.PathValue("project")+"/") && strings.Contains(repo, fuzzyMatch(r)) && len(repos) < fakePageSize {
				repos = append(repos, map[string]string{"name": repo})
			}
		}
		_ = json.NewEncoder(w).Encode(repos)
	})
	mux.HandleFunc("DELETE /api/v2.0/projects/{project}/repositories/{name}", func(w http.ResponseWriter, r *http.Request) {
		// Like Harbor, unescape the name again, so a name containing "/" must be encoded twice.
		escaped := r.PathValue("name")
		name, err := url.PathUnescape(escaped)
		h.mu.Lock()
		defer h.mu.Unlock()
		i := slices.Index(h.repositories, r.PathValue("project")+"/"+name)
		if err != nil || strings.Contains(escaped, "/") || i < 0 {
			http.NotFound(w, r)
			return
		}
		h.repositories = slices.Delete(h.repositories, i, i+1)
	})
	mux.HandleFunc("GET /api/v2.0/replication/policies", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		policies := []fakePolicy{}
		for _, p := range h.policies {
			if strings.Contains(p.Name, fuzzyMatch(r)) && len(policies) < fakePageSize {
				policies = append(policies, p)
			}
		}
		_ = json.NewEncoder(w).Encode(policies)
	})
	mux.HandleFunc("DELETE /api/v2.0/replication/policies/{id}", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		id := pathID(r)
		if slices.ContainsFunc(h.executions, func(e fakeExecution) bool { return e.PolicyID == id && e.Status == "InProgress" }) {
			http.Error(w, "the policy is running", http.StatusPreconditionFailed)
			return
		}
		i := slices.IndexFunc(h.policies, func(p fakePolicy) bool { return p.ID == id })
		if i < 0 {
			http.NotFound(w, r)
			return
		}
		h.policies = slices.Delete(h.policies, i, i+1)
	})
	mux.HandleFunc("GET /api/v2.0/replication/executions", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		executions := []fakeExecution{}
		for _, e := range h.executions {
			if strconv.FormatInt(e.PolicyID, 10) == r.URL.Query().Get("policy_id") && e.Status == r.URL.Query().Get("status") {
				executions = append(executions, e)
			}
		}
		_ = json.NewEncoder(w).Encode(executions)
	})
	mux.HandleFunc("PUT /api/v2.0/replication/executions/{id}", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		i := slices.IndexFunc(h.executions, func(e fakeExecution) bool { return e.ID == pathID(r) })
		if i < 0 {
			http.NotFound(w, r)
			return
		}
		h.executions[i].Status = "Stopped"
	})
	mux.HandleFunc("POST /api/v2.0/registries", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var registry struct{ Name string }
		_ = json.Unmarshal(body, &registry)
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.registries[registry.Name]; ok {
			http.Error(w, "the registry exists", http.StatusConflict)
			return
		}
		h.registries[registry.Name] = body
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("GET /api/v2.0/robots", func(w http.ResponseWriter, r *http.Request) {
		// Like Harbor, unescape q again after parsing the query string.
		q, _ := url.QueryUnescape(r.URL.Query().Get("q"))
		switch q {
		case "Level=project,ProjectID=3,name=e2e+harbor-apiserver":
			_, _ = io.WriteString(w, `[{"id": 7, "name": "robot$e2e+harbor-apiserver"}]`)
		case "Level=system,name=harbor-apiserver-replication":
			_, _ = io.WriteString(w, `[{"id": 9, "name": "robot$harbor-apiserver-replication"}]`)
		default:
			_, _ = io.WriteString(w, `[]`)
		}
	})
	mux.HandleFunc("DELETE /api/v2.0/robots/7", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("DELETE /api/v2.0/robots/9", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("POST /api/v2.0/robots", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var robot struct{ Name, Level string }
		_ = json.Unmarshal(body, &robot)
		h.mu.Lock()
		h.robotRequests[robot.Name] = body
		h.mu.Unlock()
		name := "robot$" + robot.Name
		if robot.Level == "project" {
			name = "robot$e2e+" + robot.Name
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id": 8, "name": "`+name+`", "secret": "s3cret"}`)
	})
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.requests = append(h.requests, r.Method+" "+r.URL.EscapedPath())
		stalled := h.stalled != "" && strings.HasPrefix(r.URL.Path, h.stalled)
		h.mu.Unlock()
		if stalled {
			// The server notices that the client went away only after the body is read.
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
				// Fail a client that never gives up, rather than hang the test.
				http.Error(w, "stalled", http.StatusBadRequest)
			}
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(h.Close)
	return h
}

// stall makes requests whose path starts with prefix hang until the client gives up.
func (h *fakeHarbor) stall(prefix string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stalled = prefix
}

func (h *fakeHarbor) requestLog() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.requests...)
}

func (h *fakeHarbor) robotRequest(name string) []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.robotRequests[name]
}

// state returns the repositories and the names of the policies.
func (h *fakeHarbor) state() (repositories, policies []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.policies {
		policies = append(policies, p.Name)
	}
	return slices.Clone(h.repositories), policies
}

func digest(t *testing.T, x interface{ Digest() (v1.Hash, error) }) string {
	t.Helper()
	d, err := x.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d.String()
}

func TestReplaceRepositories(t *testing.T) {
	h := newFakeHarbor(t, "e2e/stale", "e2e/team/api", "e2e/app", "e2e-source/stale")
	seed := NewSeed()

	if err := NewAdmin(h.URL).ReplaceRepositories(t.Context(), seed); err != nil {
		t.Fatal(err)
	}

	wantFirst := []string{
		"GET /api/v2.0/replication/policies",
		"POST /api/v2.0/projects",
		"GET /api/v2.0/projects/e2e/repositories",
		"DELETE /api/v2.0/projects/e2e/repositories/stale",
		"DELETE /api/v2.0/projects/e2e/repositories/team%252Fapi",
		"GET /api/v2.0/projects/e2e/repositories",
		"DELETE /api/v2.0/projects/e2e/repositories/app",
		"GET /api/v2.0/projects/e2e/repositories",
	}
	got := h.requestLog()
	if diff := cmp.Diff(wantFirst, got[:min(len(wantFirst), len(got))]); diff != "" {
		t.Errorf("first requests (-want +got):\n%s", diff)
	}
	if repositories, _ := h.state(); len(repositories) > 0 {
		t.Errorf("repositories left: %v", repositories)
	}
	if diff := cmp.Diff([]string{"e2e", "e2e-source"}, h.projects); diff != "" {
		t.Errorf("created projects (-want +got):\n%s", diff)
	}

	host := strings.TrimPrefix(h.URL, "http://")
	for ref, want := range map[string]string{
		"e2e/app:v1":                          digest(t, seed.App),
		"e2e/app:latest":                      digest(t, seed.App),
		"e2e/app@" + digest(t, seed.Untagged): digest(t, seed.Untagged),
		"e2e/multi:v1":                        digest(t, seed.Multi),
		"e2e/team/api:v1":                     digest(t, seed.TeamAPI),
		"e2e/dotted.name_x:v1":                digest(t, seed.Dotted),
		"e2e-source/team/app:v1":              digest(t, seed.SourceV1),
		"e2e-source/team/app:v2":              digest(t, seed.SourceV2),
	} {
		r, err := name.ParseReference(host + "/" + ref)
		if err != nil {
			t.Fatal(err)
		}
		d, err := remote.Head(r)
		if err != nil {
			t.Errorf("%s: %v", ref, err)
			continue
		}
		if d.Digest.String() != want {
			t.Errorf("%s: got %s, want %s", ref, d.Digest, want)
		}
	}
	if digest(t, seed.SourceV1) == digest(t, seed.SourceV2) {
		t.Error("source images v1 and v2 have the same digest")
	}
	app, err := name.NewRepository(host + "/e2e/app")
	if err != nil {
		t.Fatal(err)
	}
	tags, err := remote.List(app)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"latest", "v1"}, tags); diff != "" {
		t.Errorf("e2e/app tags (-want +got):\n%s", diff)
	}
}

func TestReplaceRepositoriesFirstDeletesLeftoverReplications(t *testing.T) {
	defer func(d time.Duration) { stopRetryInterval = d }(stopRetryInterval)
	stopRetryInterval = time.Millisecond
	h := newFakeHarbor(t, "e2e/k8s/ns1/web/e2e-source/team/app")
	h.policies = []fakePolicy{{1, "k8s.e2e.ns1.web"}, {2, "old.k8s.e2e.ns1.web"}}
	h.executions = []fakeExecution{{21, 1, "InProgress"}, {22, 2, "InProgress"}}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	if err := NewAdmin(h.URL).ReplaceRepositories(ctx, NewSeed()); err != nil {
		t.Fatal(err)
	}

	wantFirst := []string{
		"GET /api/v2.0/replication/policies",
		"DELETE /api/v2.0/replication/policies/1",
		"GET /api/v2.0/replication/executions",
		"PUT /api/v2.0/replication/executions/21",
		"DELETE /api/v2.0/replication/policies/1",
		"GET /api/v2.0/replication/policies",
		"POST /api/v2.0/projects",
	}
	got := h.requestLog()
	if diff := cmp.Diff(wantFirst, got[:min(len(wantFirst), len(got))]); diff != "" {
		t.Errorf("first requests (-want +got):\n%s", diff)
	}
	repositories, policies := h.state()
	if len(repositories) > 0 || !slices.Equal(policies, []string{"old.k8s.e2e.ns1.web"}) {
		t.Errorf("repositories %v, policies %v", repositories, policies)
	}
	if h.executions[1].Status != "InProgress" {
		t.Errorf("stopped another policy's execution: %+v", h.executions[1])
	}
}

func TestDeleteReplications(t *testing.T) {
	h := newFakeHarbor(t,
		"e2e/app",
		"e2e/k8s/ns1/web/e2e-source/team/app",
		"e2e/k8s/ns1/api/x",
		"e2e/k8s/ns10/web/x",
		"e2e-source/k8s/ns1/web/x",
		"e2e/old/e2e/k8s/ns1/x",
	)
	h.policies = []fakePolicy{{1, "k8s.e2e.ns1.web"}, {2, "k8s.e2e.ns10.web"}}

	if err := NewAdmin(h.URL).deleteReplications(t.Context(), "ns1"); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"GET /api/v2.0/replication/policies",
		"DELETE /api/v2.0/replication/policies/1",
		"GET /api/v2.0/replication/policies",
		"GET /api/v2.0/projects/e2e/repositories",
		"DELETE /api/v2.0/projects/e2e/repositories/k8s%252Fns1%252Fweb%252Fe2e-source%252Fteam%252Fapp",
		"DELETE /api/v2.0/projects/e2e/repositories/k8s%252Fns1%252Fapi%252Fx",
		"GET /api/v2.0/projects/e2e/repositories",
	}
	if diff := cmp.Diff(want, h.requestLog()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
	repositories, policies := h.state()
	if diff := cmp.Diff([]string{"e2e/app", "e2e/k8s/ns10/web/x", "e2e-source/k8s/ns1/web/x", "e2e/old/e2e/k8s/ns1/x"}, repositories); diff != "" {
		t.Errorf("repositories (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"k8s.e2e.ns10.web"}, policies); diff != "" {
		t.Errorf("policies (-want +got):\n%s", diff)
	}
}

func TestCreateRegistries(t *testing.T) {
	h := newFakeHarbor(t)
	admin := NewAdmin(h.URL)

	for range 2 {
		if err := admin.CreateRegistries(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	for _, registry := range []string{"e2e-harbor", "e2e-unlisted"} {
		var got, want any
		if err := json.Unmarshal(h.registries[registry], &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(`{
			"name": "`+registry+`",
			"type": "harbor",
			"url": "http://harbor.harbor.svc",
			"insecure": true,
			"credential": {"type": "basic", "access_key": "admin", "access_secret": "Harbor12345"}
		}`), &want); err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("%s (-want +got):\n%s", registry, diff)
		}
	}
}

func TestAdminGivesUpOnStalledHarbor(t *testing.T) {
	defer func(d time.Duration) { requestTimeout = d }(requestTimeout)
	for _, tc := range []struct {
		name, stalled            string
		deadline, requestTimeout time.Duration
	}{
		{"API at context deadline", "/api/", 200 * time.Millisecond, time.Minute},
		{"registry at context deadline", "/v2/", 200 * time.Millisecond, time.Minute},
		{"API at request timeout", "/api/", time.Minute, 200 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requestTimeout = tc.requestTimeout
			h := newFakeHarbor(t)
			h.stall(tc.stalled)
			ctx, cancel := context.WithTimeout(t.Context(), tc.deadline)
			defer cancel()

			err := NewAdmin(h.URL).ReplaceRepositories(ctx, NewSeed())

			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("got %v, want %v", err, context.DeadlineExceeded)
			}
		})
	}
}

func TestCreateRobotReplacesExistingRobot(t *testing.T) {
	h := newFakeHarbor(t)

	username, password, err := NewAdmin(h.URL).CreateRobot(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if username != "robot$e2e+harbor-apiserver" || password != "s3cret" {
		t.Errorf("got %q, %q", username, password)
	}
	want := []string{
		"GET /api/v2.0/projects/e2e",
		"GET /api/v2.0/robots",
		"DELETE /api/v2.0/robots/7",
		"POST /api/v2.0/robots",
	}
	if diff := cmp.Diff(want, h.requestLog()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

func TestCreateRobotGrantsOnlyReadAccessToTheProject(t *testing.T) {
	h := newFakeHarbor(t)

	if _, _, err := NewAdmin(h.URL).CreateRobot(t.Context()); err != nil {
		t.Fatal(err)
	}

	var got, want any
	if err := json.Unmarshal(h.robotRequest("harbor-apiserver"), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{
		"name": "harbor-apiserver",
		"level": "project",
		"duration": -1,
		"permissions": [{
			"kind": "project",
			"namespace": "e2e",
			"access": [
				{"resource": "repository", "action": "list"},
				{"resource": "artifact", "action": "list"}
			]
		}]
	}`), &want); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("robot (-want +got):\n%s", diff)
	}
}

func TestCreateReplicationRobotReplacesExistingRobot(t *testing.T) {
	h := newFakeHarbor(t)

	username, password, err := NewAdmin(h.URL).CreateReplicationRobot(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if username != "robot$harbor-apiserver-replication" || password != "s3cret" {
		t.Errorf("got %q, %q", username, password)
	}
	want := []string{
		"GET /api/v2.0/robots",
		"DELETE /api/v2.0/robots/9",
		"POST /api/v2.0/robots",
	}
	if diff := cmp.Diff(want, h.requestLog()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

func TestCreateReplicationRobotGrantsOnlyReplicationAccess(t *testing.T) {
	h := newFakeHarbor(t)

	if _, _, err := NewAdmin(h.URL).CreateReplicationRobot(t.Context()); err != nil {
		t.Fatal(err)
	}

	var got, want any
	if err := json.Unmarshal(h.robotRequest("harbor-apiserver-replication"), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{
		"name": "harbor-apiserver-replication",
		"level": "system",
		"duration": -1,
		"permissions": [{
			"kind": "system",
			"namespace": "/",
			"access": [
				{"resource": "registry", "action": "list"},
				{"resource": "replication-policy", "action": "list"},
				{"resource": "replication-policy", "action": "read"},
				{"resource": "replication-policy", "action": "create"},
				{"resource": "replication-policy", "action": "delete"},
				{"resource": "replication", "action": "list"},
				{"resource": "replication", "action": "create"}
			]
		}]
	}`), &want); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("robot (-want +got):\n%s", diff)
	}
}
