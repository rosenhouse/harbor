//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/yaml"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

const projectLabel = "harbor.goharbor.io/project"

func label(t *testing.T, ns, project string) {
	t.Helper()
	if project == "" {
		mustKubectl(t, "label", "namespace", ns, projectLabel+"-")
		return
	}
	mustKubectl(t, "label", "namespace", ns, "--overwrite", projectLabel+"="+project)
}

func listRepositories(t *testing.T, args ...string) ([]v1alpha1.HarborRepository, error) {
	t.Helper()
	out, err := kubectl(t, append([]string{"get", "harborrepositories", "-o", "json"}, args...)...)
	if err != nil {
		return nil, err
	}
	var list v1alpha1.HarborRepositoryList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// harborNames maps the repositories to their full names in Harbor, sorted.
func harborNames(repos []v1alpha1.HarborRepository) []string {
	var names []string
	for _, r := range repos {
		names = append(names, r.Status.Name)
	}
	slices.Sort(names)
	return names
}

// expectRepositories waits until the namespace lists the Harbor repositories named want.
func expectRepositories(t *testing.T, ns string, want []string) []v1alpha1.HarborRepository {
	t.Helper()
	var repos []v1alpha1.HarborRepository
	eventually(t, func() error {
		var err error
		if repos, err = listRepositories(t, "-n", ns); err != nil {
			return err
		}
		if diff := cmp.Diff(want, harborNames(repos)); diff != "" {
			return fmt.Errorf("repositories in %s (-want +got):\n%s", ns, diff)
		}
		return nil
	})
	return repos
}

func TestRepositories(t *testing.T) {
	ns := newNamespace(t)
	label(t, ns, HarborProject)
	repos := expectRepositories(t, ns, seededRepositories)

	byHarborName := map[string]v1alpha1.HarborRepository{}
	for _, r := range repos {
		byHarborName[r.Status.Name] = r
		if r.Namespace != ns || r.UID == "" || r.CreationTimestamp.IsZero() {
			t.Errorf("%s: namespace %q, uid %q, created %v", r.Name, r.Namespace, r.UID, r.CreationTimestamp)
		}
	}
	if r := byHarborName["e2e/team/api"]; r.Name != "team.api" || r.Status.ArtifactCount != 1 {
		t.Errorf("team/api: name %q, %d artifacts", r.Name, r.Status.ArtifactCount)
	}
	if r := byHarborName["e2e/app"]; r.Status.ArtifactCount != 2 {
		t.Errorf("app: %d artifacts, want 2", r.Status.ArtifactCount)
	}
	dotted := byHarborName["e2e/dotted.name_x"].Name
	if !strings.HasPrefix(dotted, "dotted.name-x-") {
		t.Errorf("dotted.name_x: name %q", dotted)
	}

	// Each replica has its own namespace informer, so the replica serving a later request may not see the label yet.
	for name, harborName := range map[string]string{"team.api": "e2e/team/api", dotted: "e2e/dotted.name_x"} {
		eventually(t, func() error {
			out, err := kubectl(t, "-n", ns, "get", "harborrepository", name, "-o", "yaml")
			if err != nil {
				return err
			}
			var r v1alpha1.HarborRepository
			if err := yaml.Unmarshal([]byte(out), &r); err != nil {
				return err
			}
			if r.Status.Name != harborName {
				return fmt.Errorf("get %s: status.name %q, want %q", name, r.Status.Name, harborName)
			}
			return nil
		})
	}
	if _, err := kubectl(t, "-n", ns, "get", "harborrepository", "missing"); err == nil || !strings.Contains(err.Error(), "NotFound") {
		t.Errorf("get missing: %v", err)
	}

	for format, want := range map[string][]string{
		"":     {"NAME", "ARTIFACTS", "PULLS", "AGE"},
		"wide": {"NAME", "REPOSITORY", "ARTIFACTS", "PULLS", "AGE"},
	} {
		args := []string{"-n", ns, "get", "harborrepositories"}
		if format != "" {
			args = append(args, "-o", format)
		}
		eventually(t, func() error {
			out, err := kubectl(t, args...)
			if err != nil {
				return err
			}
			header, _, _ := strings.Cut(out, "\n")
			if diff := cmp.Diff(want, strings.Fields(header)); diff != "" {
				return fmt.Errorf("-o %q header (-want +got):\n%s", format, diff)
			}
			return nil
		})
	}
}

func TestNamespaceLabelControlsVisibility(t *testing.T) {
	ns := newNamespace(t)
	expect := func(project string, want []string) {
		t.Helper()
		label(t, ns, project)
		expectRepositories(t, ns, want)
		eventually(t, func() error {
			all, err := listRepositories(t, "--all-namespaces")
			if err != nil {
				return err
			}
			listed := slices.ContainsFunc(all, func(r v1alpha1.HarborRepository) bool { return r.Namespace == ns })
			if listed != (len(want) > 0) {
				return fmt.Errorf("labeled %q: all-namespaces list includes %s: %v", project, ns, listed)
			}
			return nil
		})
	}

	expect(HarborProject, seededRepositories)
	expect("some-other-project", nil)
	expect(HarborProject, seededRepositories)
	expect("", nil)
}

func TestHarborOutageIsServiceUnavailable(t *testing.T) {
	ns := newNamespace(t)
	label(t, ns, HarborProject)
	expectRepositories(t, ns, seededRepositories)
	available := apiServiceAvailable(t)
	if !strings.HasPrefix(available, "True ") {
		t.Fatalf("APIService Available condition %q before the Harbor outage", available)
	}

	const nginx = "deployment/harbor-nginx"
	replicas := mustKubectl(t, "-n", "harbor", "get", nginx, "-o", "jsonpath={.spec.replicas}")
	t.Cleanup(func() {
		eventually(t, func() error {
			_, err := kubectl(t, "-n", "harbor", "scale", nginx, "--replicas="+replicas)
			return err
		})
		eventually(t, func() error {
			_, err := kubectl(t, "-n", "harbor", "rollout", "status", nginx, "--timeout=3m")
			return err
		})
		expectRepositories(t, ns, seededRepositories)
	})
	mustKubectl(t, "-n", "harbor", "scale", nginx, "--replicas=0")

	unavailable := func() error {
		_, err := kubectl(t, "-n", ns, "get", "harborrepositories")
		if err == nil || !strings.Contains(err.Error(), "ServiceUnavailable") || !strings.Contains(err.Error(), harbor.ErrUnavailable.Error()) {
			return fmt.Errorf("got %v, want ServiceUnavailable from harbor-apiserver", err)
		}
		return nil
	}
	eventually(t, unavailable)

	// A readiness probe that depended on Harbor would fail within 30s (see deploy/deployment.yaml).
	time.Sleep(40 * time.Second)
	eventually(t, unavailable)
	if after := apiServiceAvailable(t); after != available {
		t.Errorf("APIService Available condition changed from %q to %q during the Harbor outage", available, after)
	}
}

// apiServiceAvailable returns the status and last transition time of the APIService's Available condition.
func apiServiceAvailable(t *testing.T) string {
	t.Helper()
	var out string
	eventually(t, func() error {
		var err error
		out, err = kubectl(t, "get", "apiservice", "v1alpha1.harbor.goharbor.io", "-o", `jsonpath={.status.conditions[?(@.type=="Available")]['status','lastTransitionTime']}`)
		return err
	})
	return out
}
