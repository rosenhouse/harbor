package harbor_test

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func newClient(t *testing.T, f *fakeHarbor, opts ...harbor.Option) *harbor.Client {
	t.Helper()
	c, err := harbor.NewClient(f.URL+"/", "robot$proj+k8s", "secret", f.Client(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestListRepositoriesPages(t *testing.T) {
	f := newFakeHarbor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Total-Count", "3")
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = io.WriteString(w, `[{"name":"proj/a","artifact_count":2,"pull_count":7,"creation_time":"2026-01-02T03:04:05Z"},{"name":"proj/b"}]`)
		case "2":
			_, _ = io.WriteString(w, `[{"name":"proj/team/c","description":"d"}]`)
		default:
			_, _ = io.WriteString(w, `[]`)
		}
	})

	repos, err := newClient(t, f, harbor.WithPageSize(2)).ListRepositories(context.Background(), "proj")
	if err != nil {
		t.Fatal(err)
	}

	wantRequests := []string{
		"/api/v2.0/projects/proj/repositories?page=1&page_size=2",
		"/api/v2.0/projects/proj/repositories?page=2&page_size=2",
	}
	if diff := cmp.Diff(wantRequests, f.requests); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
	want := []harbor.Repository{
		{Name: "proj/a", ArtifactCount: 2, PullCount: 7, CreationTime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{Name: "proj/b"},
		{Name: "proj/team/c", Description: "d"},
	}
	if diff := cmp.Diff(want, repos); diff != "" {
		t.Errorf("repositories (-want +got):\n%s", diff)
	}
}

func TestListStopsAfterLastPage(t *testing.T) {
	for name, totalCount := range map[string]string{
		"full page reaching X-Total-Count": "2",
		"short page without X-Total-Count": "",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeHarbor(t, func(w http.ResponseWriter, r *http.Request) {
				if totalCount != "" {
					w.Header().Set("X-Total-Count", totalCount)
					_, _ = io.WriteString(w, `[{"name":"proj/a"},{"name":"proj/b"}]`)
					return
				}
				_, _ = io.WriteString(w, `[{"name":"proj/a"}]`)
			})
			if _, err := newClient(t, f, harbor.WithPageSize(2)).ListRepositories(context.Background(), "proj"); err != nil {
				t.Fatal(err)
			}
			if len(f.requests) != 1 {
				t.Errorf("made %d requests, want 1", len(f.requests))
			}
		})
	}
}

func TestPathsEncodeNestedRepositoryNamesTwice(t *testing.T) {
	f := newFakeHarbor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Total-Count", "0")
		_, _ = io.WriteString(w, `{}`)
	})
	c := newClient(t, f)
	ctx := context.Background()
	digest := "sha256:" + fmt.Sprintf("%064d", 1)

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
			"digest": "sha256:abc",
			"repository_name": "proj/team/app",
			"type": "IMAGE",
			"media_type": "application/vnd.oci.image.config.v1+json",
			"manifest_media_type": "application/vnd.oci.image.index.v1+json",
			"size": 1024,
			"push_time": "2026-01-02T03:04:05Z",
			"annotations": {"org.opencontainers.image.source": "https://example.com"},
			"references": [{"child_digest": "sha256:def", "platform": {"architecture": "arm64", "os": "linux"}}],
			"tags": [{"name": "v1", "push_time": "2026-01-02T03:04:05Z", "immutable": true}]
		}]`)
	})

	artifacts, err := newClient(t, f).ListArtifacts(context.Background(), "proj", "team/app")
	if err != nil {
		t.Fatal(err)
	}

	if want := "/api/v2.0/projects/proj/repositories/team%252Fapp/artifacts?page=1&page_size=100&with_tag=true"; f.requests[0] != want {
		t.Errorf("request %s, want %s", f.requests[0], want)
	}
	pushed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	want := []harbor.Artifact{{
		Digest:            "sha256:abc",
		RepositoryName:    "proj/team/app",
		Type:              "IMAGE",
		MediaType:         "application/vnd.oci.image.config.v1+json",
		ManifestMediaType: "application/vnd.oci.image.index.v1+json",
		Size:              1024,
		PushTime:          pushed,
		Annotations:       map[string]string{"org.opencontainers.image.source": "https://example.com"},
		References:        []harbor.Reference{{ChildDigest: "sha256:def", Platform: &harbor.Platform{Architecture: "arm64", OS: "linux"}}},
		Tags:              []harbor.Tag{{Name: "v1", PushTime: pushed, Immutable: true}},
	}}
	if diff := cmp.Diff(want, artifacts); diff != "" {
		t.Errorf("artifacts (-want +got):\n%s", diff)
	}
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusNotFound, harbor.ErrNotFound},
		{http.StatusUnauthorized, harbor.ErrCredentialsRejected},
		{http.StatusForbidden, harbor.ErrCredentialsRejected},
		{http.StatusInternalServerError, harbor.ErrUnavailable},
		{http.StatusServiceUnavailable, harbor.ErrUnavailable},
		{http.StatusTooManyRequests, harbor.ErrUnavailable},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			f := newFakeHarbor(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"errors":[{"code":"X","message":"harbor says no"}]}`)
			})
			_, err := newClient(t, f).GetRepository(context.Background(), "proj", "a")
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
			want := fmt.Sprintf("%v: GET /api/v2.0/projects/proj/repositories/a: %d %s: harbor says no", tc.want, tc.status, http.StatusText(tc.status))
			if err == nil || err.Error() != want {
				t.Errorf("error %q, want %q", err, want)
			}
		})
	}
}

func TestNewClientRequiresHTTPURL(t *testing.T) {
	for _, u := range []string{"harbor.example.com", "ftp://harbor.example.com", "://"} {
		if _, err := harbor.NewClient(u, "u", "p", http.DefaultClient); err == nil {
			t.Errorf("accepted %q", u)
		}
	}
}

func TestWrongCredentials(t *testing.T) {
	f := newFakeHarbor(t, func(w http.ResponseWriter, r *http.Request) {})
	c, err := harbor.NewClient(f.URL, "robot$proj+k8s", "wrong", f.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListRepositories(context.Background(), "proj"); !errors.Is(err, harbor.ErrCredentialsRejected) {
		t.Errorf("got %v", err)
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

func TestHTTPClientTrustsCABundle(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(s.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})

	trusting, err := harbor.NewHTTPClient(ca, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trusting.Get(s.URL); err != nil {
		t.Errorf("with CA bundle: %v", err)
	}

	system, err := harbor.NewHTTPClient(nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := system.Get(s.URL); err == nil {
		t.Error("without CA bundle: trusted a self-signed certificate")
	}
	if system.Timeout != time.Second {
		t.Errorf("timeout %v", system.Timeout)
	}

	if _, err := harbor.NewHTTPClient([]byte("not a certificate"), time.Second); err == nil {
		t.Error("accepted an invalid CA bundle")
	}
}
