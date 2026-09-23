//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestMain waits for kubectl to discover the API, which kube-apiserver serves shortly after the APIService becomes available.
func TestMain(m *testing.M) {
	deadline := time.Now().Add(time.Minute)
	for {
		out, err := exec.Command("kubectl", "api-resources", "--api-group=harbor.goharbor.io", "-o", "name").Output()
		if strings.Contains(string(out), "harborartifacts.harbor.goharbor.io") {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "kubectl did not discover harborartifacts: %q, %v\n", out, err)
			os.Exit(1)
		}
		time.Sleep(time.Second)
	}
	os.Exit(m.Run())
}
