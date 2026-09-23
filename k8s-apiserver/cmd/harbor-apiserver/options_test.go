package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apiserver"
)

// fakeKubeAPIServer authenticates "alice-token" as alice and authorizes only alice.
func fakeKubeAPIServer(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var resp any
		switch r.URL.Path {
		case "/apis/authentication.k8s.io/v1/tokenreviews":
			var review authenticationv1.TokenReview
			_ = json.NewDecoder(r.Body).Decode(&review)
			if review.Spec.Token == "alice-token" {
				review.Status = authenticationv1.TokenReviewStatus{Authenticated: true, User: authenticationv1.UserInfo{Username: "alice"}}
			}
			resp = review
		case "/apis/authorization.k8s.io/v1/subjectaccessreviews":
			var review authorizationv1.SubjectAccessReview
			_ = json.NewDecoder(r.Body).Decode(&review)
			review.Status.Allowed = review.Spec.User == "alice"
			resp = review
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(s.Close)

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	err := os.WriteFile(kubeconfig, []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters: [{name: fake, cluster: {server: %q}}]
users: [{name: fake, user: {token: server-token}}]
contexts: [{name: fake, context: {cluster: fake, user: fake}}]
current-context: fake
`, s.URL)), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	return kubeconfig
}

func TestDelegatesAuthenticationAndAuthorization(t *testing.T) {
	kubeconfig := fakeKubeAPIServer(t)
	workDir := t.TempDir()
	t.Chdir(workDir)

	o := newOptions()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	o.SecureServing.Listener = listener
	o.Authentication.RemoteKubeConfigFile = kubeconfig
	o.Authentication.SkipInClusterLookup = true
	o.Authorization.RemoteKubeConfigFile = kubeconfig
	c, err := o.config()
	if err != nil {
		t.Fatal(err)
	}
	s, err := apiserver.New(c.Complete(nil))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		token string
		want  int
	}{
		{"alice-token", http.StatusOK},
		{"bob-token", http.StatusUnauthorized},
		{"", http.StatusForbidden},
	} {
		req := httptest.NewRequest(http.MethodGet, "/apis/harbor.goharbor.io/v1alpha1/harborrepositories", nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		rec := httptest.NewRecorder()
		s.Handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("token %q: status %d, want %d", tc.token, rec.Code, tc.want)
		}
	}

	if files, _ := os.ReadDir(workDir); len(files) != 0 {
		t.Errorf("wrote %v; want the self-signed certificate kept in memory", files)
	}
}
