package harbor_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

// fakeHarbor serves handler behind robot credentials and records each request URI.
type fakeHarbor struct {
	*httptest.Server
	requests []string
}

func newFakeHarbor(t *testing.T, handler http.HandlerFunc) *fakeHarbor {
	t.Helper()
	f := &fakeHarbor{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.RequestURI)
		if user, pass, ok := r.BasicAuth(); !ok || user != "robot$proj+k8s" || pass != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func newClient(t *testing.T, f *fakeHarbor) *harbor.Client {
	t.Helper()
	c, err := harbor.NewClient(f.URL+"/", "robot$proj+k8s", "secret", f.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// repositories returns n repositories with IDs from first.
func repositories(first, n int) []harbor.Repository {
	var r []harbor.Repository
	for i := first; i < first+n; i++ {
		r = append(r, harbor.Repository{ID: int64(i), Name: fmt.Sprintf("proj/r%d", i)})
	}
	return r
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Error(err)
	}
}

func TestListRepositoriesPages(t *testing.T) {
	f := newFakeHarbor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Total-Count", "150")
		switch r.URL.Query().Get("page") {
		case "1":
			writeJSON(t, w, repositories(1, 100))
		case "2":
			// A repository created during the list shifted page 2 by one.
			writeJSON(t, w, repositories(100, 51))
		default:
			t.Errorf("requested %s", r.RequestURI)
		}
	})

	repos, err := newClient(t, f).ListRepositories(context.Background(), "proj")
	if err != nil {
		t.Fatal(err)
	}

	wantRequests := []string{
		"/api/v2.0/projects/proj/repositories?page=1&page_size=100&sort=repository_id",
		"/api/v2.0/projects/proj/repositories?page=2&page_size=100&sort=repository_id",
	}
	if diff := cmp.Diff(wantRequests, f.requests); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(repositories(1, 150), repos); diff != "" {
		t.Errorf("repositories (-want +got):\n%s", diff)
	}
}

func TestListStopsAfterLastPage(t *testing.T) {
	for name, tc := range map[string]struct {
		totalCount string
		n          int
	}{
		"full page reaching X-Total-Count": {"100", 100},
		"short page without X-Total-Count": {"", 99},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeHarbor(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") != "1" {
					t.Errorf("requested %s", r.RequestURI)
					writeJSON(t, w, []any{})
					return
				}
				if tc.totalCount != "" {
					w.Header().Set("X-Total-Count", tc.totalCount)
				}
				writeJSON(t, w, repositories(1, tc.n))
			})
			repos, err := newClient(t, f).ListRepositories(context.Background(), "proj")
			if err != nil || len(repos) != tc.n {
				t.Errorf("got %d repositories, %v", len(repos), err)
			}
		})
	}
}

func TestListGivesUpOnEndlessPages(t *testing.T) {
	defer harbor.SetLimits(3, 1<<20)()
	page := 0
	f := newFakeHarbor(t, func(w http.ResponseWriter, r *http.Request) {
		page++
		writeJSON(t, w, repositories(page*100, 100))
	})
	if _, err := newClient(t, f).ListRepositories(context.Background(), "proj"); err == nil || len(f.requests) != 3 {
		t.Errorf("got %v after %d requests", err, len(f.requests))
	}
}

func TestResponseSizeLimit(t *testing.T) {
	defer harbor.SetLimits(1000, 100)()
	f := newFakeHarbor(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, harbor.Repository{Name: strings.Repeat("x", 200)})
	})
	if _, err := newClient(t, f).GetRepository(context.Background(), "proj", "x"); err == nil {
		t.Error("decoded a response over the size limit")
	}
}

func TestPathsEncodeNestedRepositoryNamesTwice(t *testing.T) {
	f := newFakeHarbor(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	c := newClient(t, f)
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("0", 64)

	_, _ = c.GetRepository(ctx, "proj", "team/app")
	_, _ = c.GetArtifact(ctx, "proj", "team/app", digest)

	want := []string{
		"/api/v2.0/projects/proj/repositories/team%252Fapp",
		"/api/v2.0/projects/proj/repositories/team%252Fapp/artifacts/" + digest + "?with_tag=true",
	}
	if diff := cmp.Diff(want, f.requests); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

func TestListArtifacts(t *testing.T) {
	f := newFakeHarbor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Total-Count", "1")
		_, _ = io.WriteString(w, `[{
			"id": 7,
			"digest": "sha256:abc",
			"repository_name": "proj/team/app",
			"type": "IMAGE",
			"media_type": "application/vnd.oci.image.config.v1+json",
			"manifest_media_type": "application/vnd.oci.image.index.v1+json",
			"size": 1024,
			"push_time": "2026-01-02T03:04:05.000Z",
			"pull_time": "0001-01-01T00:00:00.000Z",
			"annotations": {"org.opencontainers.image.source": "https://example.com"},
			"references": [{"child_digest": "sha256:def", "platform": {"architecture": "arm64", "os": "linux"}}],
			"tags": [{"name": "v1", "push_time": "2026-01-02T03:04:05.000Z"}]
		}]`)
	})

	artifacts, err := newClient(t, f).ListArtifacts(context.Background(), "proj", "team/app")
	if err != nil {
		t.Fatal(err)
	}

	if want := "/api/v2.0/projects/proj/repositories/team%252Fapp/artifacts?page=1&page_size=100&sort=id&with_tag=true"; f.requests[0] != want {
		t.Errorf("request %s, want %s", f.requests[0], want)
	}
	pushed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	want := []harbor.Artifact{{
		ID:                7,
		Digest:            "sha256:abc",
		RepositoryName:    "proj/team/app",
		Type:              "IMAGE",
		MediaType:         "application/vnd.oci.image.config.v1+json",
		ManifestMediaType: "application/vnd.oci.image.index.v1+json",
		Size:              1024,
		PushTime:          pushed,
		Annotations:       map[string]string{"org.opencontainers.image.source": "https://example.com"},
		References:        []harbor.Reference{{ChildDigest: "sha256:def", Platform: &harbor.Platform{Architecture: "arm64", OS: "linux"}}},
		Tags:              []harbor.Tag{{Name: "v1", PushTime: pushed}},
	}}
	if diff := cmp.Diff(want, artifacts); diff != "" {
		t.Errorf("artifacts (-want +got):\n%s", diff)
	}
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct {
		status  int
		body    string
		want    error
		wantMsg string
	}{
		{http.StatusNotFound, `{"errors":[{"code":"NOT_FOUND","message":"no repo"}]}`, harbor.ErrNotFound, "no repo"},
		{http.StatusUnauthorized, `{"errors":[{"message":"bad creds"}]}`, harbor.ErrUnauthorized, "bad creds"},
		{http.StatusForbidden, `{"errors":[{"message":"no access"}]}`, harbor.ErrForbidden, "no access"},
		{http.StatusInternalServerError, `{"errors":[{"message":"internal server error"}]}`, harbor.ErrUnavailable, "internal server error"},
		{http.StatusServiceUnavailable, "<html>" + strings.Repeat("x", 1000), harbor.ErrUnavailable, "<html>" + strings.Repeat("x", 250)},
		{http.StatusTooManyRequests, "slow down", harbor.ErrUnavailable, "slow down"},
		{http.StatusFound, "", nil, ""},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			f := newFakeHarbor(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			repo, err := newClient(t, f).GetRepository(context.Background(), "proj", "a")
			if err == nil || repo != nil {
				t.Fatalf("got %v, %v; want only an error", repo, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
			wantMsg := fmt.Sprintf("GET /api/v2.0/projects/proj/repositories/a: %d %s", tc.status, http.StatusText(tc.status))
			if tc.wantMsg != "" {
				wantMsg += ": " + tc.wantMsg
			}
			if !strings.HasSuffix(err.Error(), wantMsg) {
				t.Errorf("error %q, want suffix %q", err, wantMsg)
			}
		})
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var elsewhere []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere = append(elsewhere, r.Header.Get("Authorization"))
	}))
	t.Cleanup(other.Close)
	f := newFakeHarbor(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	})

	if _, err := newClient(t, f).ListRepositories(context.Background(), "proj"); err == nil {
		t.Error("followed a redirect as success")
	}
	if len(elsewhere) != 0 {
		t.Errorf("sent requests to the redirect target: %v", elsewhere)
	}
}

func TestUnreachableIsUnavailable(t *testing.T) {
	f := newFakeHarbor(t, func(w http.ResponseWriter, r *http.Request) {})
	c := newClient(t, f)
	f.Close()
	if _, err := c.ListRepositories(context.Background(), "proj"); !errors.Is(err, harbor.ErrUnavailable) {
		t.Errorf("got %v", err)
	}
}

func TestContextDeadline(t *testing.T) {
	f := newFakeHarbor(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := newClient(t, f).ListRepositories(ctx, "proj")
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, harbor.ErrUnavailable) {
		t.Errorf("got %v", err)
	}
}

func TestNewClientValidation(t *testing.T) {
	for _, u := range []string{"harbor.example.com", "ftp://harbor.example.com", "https://", "https://h/harbor?x=1", "://"} {
		if _, err := harbor.NewClient(u, "u", "p", http.DefaultClient); err == nil {
			t.Errorf("accepted %q", u)
		}
	}
	if _, err := harbor.NewClient("https://h", "u", "p", nil); err == nil {
		t.Error("accepted a nil HTTP client")
	}
}

func TestHTTPClientTrustsCABundle(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(s.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})

	trusting, err := harbor.NewHTTPClient(ca, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := trusting.Get(s.URL)
	if err != nil {
		t.Errorf("with CA bundle: %v", err)
	} else {
		_ = resp.Body.Close()
	}

	system, err := harbor.NewHTTPClient(nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := system.Get(s.URL); err == nil {
		_ = resp.Body.Close()
		t.Error("without CA bundle: trusted a self-signed certificate")
	}
	if system.Timeout != time.Second {
		t.Errorf("timeout %v", system.Timeout)
	}

	if _, err := harbor.NewHTTPClient([]byte("not a certificate"), time.Second); err == nil {
		t.Error("accepted an invalid CA bundle")
	}
}
