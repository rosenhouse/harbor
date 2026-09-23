//go:build e2e

// Package e2e tests a deployed harbor-apiserver through kubectl.
package e2e

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
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

func TestAPIResources(t *testing.T) {
	out := mustKubectl(t, "api-resources", "--api-group=harbor.goharbor.io", "--no-headers", "-o", "wide")
	got := strings.Split(out, "\n")
	slices.Sort(got)
	want := []string{
		"harborartifacts true HarborArtifact [get list]",
		"harborrepositories true HarborRepository [get list]",
	}
	for i := range got {
		got[i] = strings.Join(slices.DeleteFunc(strings.Fields(got[i]), func(f string) bool {
			return f == "harbor.goharbor.io/v1alpha1"
		}), " ")
	}
	if !slices.Equal(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestListAllNamespaces(t *testing.T) {
	out := mustKubectl(t, "get", "harborrepositories,harborartifacts", "--all-namespaces", "-o", "name")
	if out != "" {
		t.Errorf("got %q, want no objects", out)
	}
}

// namespaceWithServiceAccounts creates a namespace with a "viewer" bound to the view role and a "nobody" bound to nothing.
func namespaceWithServiceAccounts(t *testing.T) string {
	t.Helper()
	ns := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	mustKubectl(t, "create", "namespace", ns)
	t.Cleanup(func() { _, _ = kubectl(t, "delete", "namespace", ns, "--wait=false") })
	mustKubectl(t, "-n", ns, "create", "serviceaccount", "viewer")
	mustKubectl(t, "-n", ns, "create", "serviceaccount", "nobody")
	mustKubectl(t, "-n", ns, "create", "rolebinding", "viewer", "--clusterrole=view", "--serviceaccount="+ns+":viewer")

	// harbor-apiserver caches denials, so wait until RBAC allows the viewer before any test asks.
	for range 50 {
		if out, _ := kubectl(t, "-n", ns, "auth", "can-i", "list", "harborrepositories.harbor.goharbor.io", "--as=system:serviceaccount:"+ns+":viewer"); out == "yes" {
			return ns
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("RBAC never allowed the viewer")
	return ""
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
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // the serving certificate is self-signed
	}}

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

func portForward(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	cmd := exec.Command("kubectl", "-n", "harbor-apiserver", "port-forward", "service/harbor-apiserver", fmt.Sprintf("%d:443", port))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for range 50 {
		if c, err := net.Dial("tcp", addr); err == nil {
			_ = c.Close()
			return "https://" + addr
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("port-forward to %s did not become ready", addr)
	return ""
}
