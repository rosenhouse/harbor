//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// kubectl returns stdout. Its error includes stderr.
func kubectl(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("kubectl", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		err = fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out)), err
}

func mustKubectl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := kubectl(t, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// eventually retries check until it succeeds, and fails the test if it never does.
func eventually(t *testing.T, check func() error) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		err := check()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func TestAPIResources(t *testing.T) {
	want := []metav1.APIResource{
		{Name: "harborartifacts", SingularName: "harborartifact", Namespaced: true, Group: "harbor.goharbor.io", Version: "v1alpha1", Kind: "HarborArtifact", Verbs: []string{"get", "list"}},
		{Name: "harborrepositories", SingularName: "harborrepository", Namespaced: true, Group: "harbor.goharbor.io", Version: "v1alpha1", Kind: "HarborRepository", Verbs: []string{"get", "list"}},
	}
	// kube-apiserver refreshes aggregated discovery shortly after the APIService becomes available.
	eventually(t, func() error {
		out, err := kubectl(t, "api-resources", "--api-group=harbor.goharbor.io", "-o", "json")
		if err != nil {
			return err
		}
		var list metav1.APIResourceList
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			return err
		}
		if diff := cmp.Diff(want, list.APIResources, cmpopts.IgnoreFields(metav1.APIResource{}, "StorageVersionHash")); diff != "" {
			return fmt.Errorf("api-resources (-want +got):\n%s", diff)
		}
		return nil
	})
}

// newNamespace creates a namespace that is deleted when the test ends.
func newNamespace(t *testing.T) string {
	t.Helper()
	ns := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	mustKubectl(t, "create", "namespace", ns)
	t.Cleanup(func() { _, _ = kubectl(t, "delete", "namespace", ns, "--wait=false") })
	return ns
}

// namespaceWithServiceAccounts creates a namespace with a "viewer" bound to the view role and a "nobody" bound to nothing.
func namespaceWithServiceAccounts(t *testing.T) string {
	t.Helper()
	ns := newNamespace(t)
	mustKubectl(t, "-n", ns, "create", "serviceaccount", "viewer")
	mustKubectl(t, "-n", ns, "create", "serviceaccount", "nobody")
	mustKubectl(t, "-n", ns, "create", "rolebinding", "viewer", "--clusterrole=view", "--serviceaccount="+ns+":viewer")

	// harbor-apiserver caches denials, so wait until RBAC allows the viewer before any test asks.
	eventually(t, func() error {
		out, err := kubectl(t, "-n", ns, "auth", "can-i", "list", "harborrepositories.harbor.goharbor.io", "--as=system:serviceaccount:"+ns+":viewer")
		if out != "yes" {
			return fmt.Errorf("can the viewer list harborrepositories? %q, %v", out, err)
		}
		return nil
	})
	return ns
}

func TestViewRoleGrantsAccess(t *testing.T) {
	ns := namespaceWithServiceAccounts(t)

	mustKubectl(t, "-n", ns, "get", "harborrepositories,harborartifacts", "--as=system:serviceaccount:"+ns+":viewer")

	_, err := kubectl(t, "-n", ns, "get", "harborrepositories", "--as=system:serviceaccount:"+ns+":nobody")
	if err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("nobody: got %v, want Forbidden", err)
	}
}

// TestDelegatedAuthorization bypasses kube-apiserver, which authorizes proxied requests itself,
// to show that harbor-apiserver authorizes each request too.
func TestDelegatedAuthorization(t *testing.T) {
	ns := namespaceWithServiceAccounts(t)
	base := portForward(t)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(secretData(t, "harbor-apiserver", "harbor-apiserver-tls", "ca.crt")) {
		t.Fatal("no CA certificate in Secret harbor-apiserver-tls")
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "harbor-apiserver.harbor-apiserver.svc"},
		},
	}

	for _, tc := range []struct {
		serviceAccount, namespace string
		want                      int
	}{
		{"viewer", ns, http.StatusOK},
		{"viewer", "default", http.StatusForbidden},
		{"nobody", ns, http.StatusForbidden},
	} {
		token := mustKubectl(t, "-n", ns, "create", "token", tc.serviceAccount)
		req, _ := http.NewRequest(http.MethodGet, base+"/apis/harbor.goharbor.io/v1alpha1/namespaces/"+tc.namespace+"/harborrepositories", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s listing in %s: got status %d, want %d", tc.serviceAccount, tc.namespace, resp.StatusCode, tc.want)
		}
	}

	resp, err := client.Get(base + "/apis/harbor.goharbor.io/v1alpha1/namespaces/" + ns + "/harborrepositories")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("anonymous: got status %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

// portForward forwards a local port to the harbor-apiserver Service and returns its URL.
func portForward(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("kubectl", "-n", "harbor-apiserver", "port-forward", "service/harbor-apiserver", ":443")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	// kubectl prints "Forwarding from 127.0.0.1:<port> -> 6443" once it listens.
	r := bufio.NewReader(stdout)
	line, err := r.ReadString('\n')
	rest, ok := strings.CutPrefix(line, "Forwarding from ")
	if err != nil || !ok {
		t.Fatalf("port-forward printed %q, %v: %s", line, err, stderr.String())
	}
	go func() { _, _ = io.Copy(io.Discard, r) }()
	addr, _, _ := strings.Cut(rest, " ")
	return "https://" + addr
}
