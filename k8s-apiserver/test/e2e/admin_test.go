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
	mu           sync.Mutex
	requests     []string
	repositories []string
	robotRequest []byte
	stalled      string
}

func newFakeHarbor(t *testing.T, repositories ...string) *fakeHarbor {
	h := &fakeHarbor{repositories: repositories}
	mux := http.NewServeMux()
	mux.Handle("/v2/", registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	mux.HandleFunc("POST /api/v2.0/projects", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("GET /api/v2.0/projects/e2e", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"project_id": 3, "name": "e2e"}`)
	})
	mux.HandleFunc("GET /api/v2.0/projects/e2e/repositories", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		repos := []map[string]string{}
		for _, repo := range h.repositories[:min(fakePageSize, len(h.repositories))] {
			repos = append(repos, map[string]string{"name": repo})
		}
		_ = json.NewEncoder(w).Encode(repos)
	})
	mux.HandleFunc("DELETE /api/v2.0/projects/e2e/repositories/{name}", func(w http.ResponseWriter, r *http.Request) {
		// Like Harbor, unescape the name again, so a name containing "/" must be encoded twice.
		escaped := r.PathValue("name")
		name, err := url.PathUnescape(escaped)
		h.mu.Lock()
		defer h.mu.Unlock()
		i := slices.Index(h.repositories, "e2e/"+name)
		if err != nil || strings.Contains(escaped, "/") || i < 0 {
			http.NotFound(w, r)
			return
		}
		h.repositories = slices.Delete(h.repositories, i, i+1)
	})
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
		body, _ := io.ReadAll(r.Body)
		var robot struct{ Name string }
		_ = json.Unmarshal(body, &robot)
		h.mu.Lock()
		h.robotRequest = body
		h.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id": 8, "name": "robot$e2e+`+robot.Name+`", "secret": "s3cret"}`)
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

func (h *fakeHarbor) lastRobotRequest() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.robotRequest
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
	h := newFakeHarbor(t, "e2e/stale", "e2e/team/api", "e2e/app")
	seed := NewSeed()

	if err := NewAdmin(h.URL).ReplaceRepositories(t.Context(), seed); err != nil {
		t.Fatal(err)
	}

	wantFirst := []string{
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
	if err := json.Unmarshal(h.lastRobotRequest(), &got); err != nil {
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
