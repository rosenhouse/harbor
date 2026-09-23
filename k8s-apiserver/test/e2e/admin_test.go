//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// fakeHarbor serves an in-memory registry and the parts of Harbor's API that seeding uses.
// It records each request's method and escaped path.
type fakeHarbor struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
}

func newFakeHarbor(t *testing.T, repositories ...string) *fakeHarbor {
	h := &fakeHarbor{}
	mux := http.NewServeMux()
	mux.Handle("/v2/", registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	mux.HandleFunc("POST /api/v2.0/projects", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("GET /api/v2.0/projects/e2e", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"project_id": 3, "name": "e2e"}`)
	})
	mux.HandleFunc("GET /api/v2.0/projects/e2e/repositories", func(w http.ResponseWriter, r *http.Request) {
		var repos []map[string]string
		for _, repo := range repositories {
			repos = append(repos, map[string]string{"name": repo})
		}
		_ = json.NewEncoder(w).Encode(repos)
	})
	mux.HandleFunc("DELETE /api/v2.0/projects/e2e/repositories/{name}", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("GET /api/v2.0/robots", func(w http.ResponseWriter, r *http.Request) {
		// Like Harbor, unescape q again after parsing the query string.
		q, _ := url.QueryUnescape(r.URL.Query().Get("q"))
		if q == "Level=project,ProjectID=3,name=e2e+harbor-apiserver" {
			_, _ = io.WriteString(w, `[{"id": 7, "name": "robot$e2e+harbor-apiserver"}]`)
			return
		}
		_, _ = io.WriteString(w, `[]`)
	})
	mux.HandleFunc("DELETE /api/v2.0/robots/7", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("POST /api/v2.0/robots", func(w http.ResponseWriter, r *http.Request) {
		var robot struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&robot)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id": 8, "name": "robot$e2e+`+robot.Name+`", "secret": "s3cret"}`)
	})
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.requests = append(h.requests, r.Method+" "+r.URL.EscapedPath())
		h.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *fakeHarbor) requestLog() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.requests...)
}

func TestPushReplacesRepositories(t *testing.T) {
	h := newFakeHarbor(t, "e2e/stale", "e2e/team/api")
	seed := NewSeed()

	if err := seed.Push(t.Context(), NewAdmin(h.URL)); err != nil {
		t.Fatal(err)
	}

	wantFirst := []string{
		"POST /api/v2.0/projects",
		"GET /api/v2.0/projects/e2e/repositories",
		"DELETE /api/v2.0/projects/e2e/repositories/stale",
		"DELETE /api/v2.0/projects/e2e/repositories/team%252Fapi",
	}
	got := h.requestLog()
	if diff := cmp.Diff(wantFirst, got[:min(len(wantFirst), len(got))]); diff != "" {
		t.Errorf("first requests (-want +got):\n%s", diff)
	}

	host := strings.TrimPrefix(h.URL, "http://")
	for ref, want := range map[string]string{
		"e2e/app:v1":                          digest(t, seed.App),
		"e2e/app:latest":                      digest(t, seed.App),
		"e2e/app@" + digest(t, seed.Untagged): digest(t, seed.Untagged),
		"e2e/multi:v1":                        digest(t, seed.Multi),
		"e2e/team/api:v1":                     digest(t, seed.TeamAPI),
		"e2e/dotted.name_x:v1":                digest(t, seed.Dotted),
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
