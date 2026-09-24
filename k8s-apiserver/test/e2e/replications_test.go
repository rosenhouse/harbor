//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
)

// replicationTimeout bounds a replication run, which takes 20 to 60 seconds, and a namespace deletion.
const replicationTimeout = 5 * time.Minute

// sourceApp is the source repository that the tests replicate. Its tags v1 and v2 are Seed.SourceV1 and Seed.SourceV2.
const sourceApp = SourceProject + "/team/app"

// replicationNamespace creates a labeled namespace, and cleans up its replications in Harbor when the test ends.
func replicationNamespace(t *testing.T) string {
	t.Helper()
	ns := newNamespace(t)
	label(t, ns, HarborProject)
	deleteReplicationsAtEnd(t, ns)
	return ns
}

// deleteReplicationsAtEnd deletes the namespace's policies, and the repositories they copied into, when the test ends.
func deleteReplicationsAtEnd(t *testing.T, ns string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := NewAdmin(HarborURL).deleteReplications(ctx, ns); err != nil {
			t.Error(err)
		}
	})
}

// replicator creates a service account bound to the view and replicate roles, and returns kubectl's flag to act as it.
func replicator(t *testing.T, ns string) string {
	t.Helper()
	mustKubectl(t, "-n", ns, "create", "serviceaccount", "replicator")
	mustKubectl(t, "-n", ns, "create", "rolebinding", "replicator-view", "--clusterrole=view", "--serviceaccount="+ns+":replicator")
	mustKubectl(t, "-n", ns, "create", "rolebinding", "replicator", "--clusterrole=harbor.goharbor.io:replicate", "--serviceaccount="+ns+":replicator")
	as := "--as=system:serviceaccount:" + ns + ":replicator"
	// harbor-apiserver caches denials, so wait until RBAC allows the replicator before any test asks.
	for _, verb := range []string{"get", "create", "patch", "delete"} {
		eventually(t, func() error {
			out, err := kubectl(t, "-n", ns, "auth", "can-i", verb, "harborreplications.harbor.goharbor.io", as)
			if out != "yes" {
				return fmt.Errorf("can the replicator %s harborreplications? %q, %v", verb, out, err)
			}
			return nil
		})
	}
	return as
}

func replicationManifest(ns, name string, spec v1alpha1.HarborReplicationSpec) map[string]any {
	return map[string]any{
		"apiVersion": v1alpha1.SchemeGroupVersion.String(),
		"kind":       "HarborReplication",
		"metadata":   map[string]string{"namespace": ns, "name": name},
		"spec":       spec,
	}
}

// manifestFile writes a manifest to a file, and returns the file's path.
func manifestFile(t *testing.T, manifest any) string {
	t.Helper()
	b, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// apply runs kubectl apply on a manifest, and returns kubectl's output.
func apply(t *testing.T, manifest any, args ...string) (string, error) {
	t.Helper()
	return kubectl(t, append([]string{"apply", "-f", manifestFile(t, manifest)}, args...)...)
}

// createReplication applies a manifest, retrying because the replica serving the create may not see the namespace's label yet.
func createReplication(t *testing.T, manifest any, args ...string) {
	t.Helper()
	eventually(t, func() error {
		_, err := apply(t, manifest, args...)
		return err
	})
}

// waitForReplication waits until the replication's last execution ends, and fails the test unless it succeeded.
func waitForReplication(t *testing.T, ns, name string) v1alpha1.HarborReplication {
	t.Helper()
	var r v1alpha1.HarborReplication
	eventuallyWithin(t, replicationTimeout, func() error {
		out, err := kubectl(t, "-n", ns, "get", "harborreplication", name, "-o", "json")
		if err != nil {
			return err
		}
		r = v1alpha1.HarborReplication{}
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			return err
		}
		if e := r.Status.LastExecution; e == nil || e.Phase == v1alpha1.ReplicationPhaseInProgress {
			return fmt.Errorf("replication %s/%s has not finished: %s", ns, name, out)
		}
		return nil
	})
	if e := r.Status.LastExecution; e.Phase != v1alpha1.ReplicationPhaseSucceeded {
		t.Fatalf("replication %s/%s ended with %+v", ns, name, *e)
	}
	return r
}

// policyNames returns the names of Harbor's replication policies for a namespace.
func policyNames(ctx context.Context, ns string) ([]string, error) {
	policies, err := NewAdmin(HarborURL).replicationPolicies(ctx, policyPrefix(ns))
	var names []string
	for _, p := range policies {
		names = append(names, p.Name)
	}
	return names, err
}

func expectPolicies(t *testing.T, ns string, want ...string) {
	t.Helper()
	got, err := policyNames(t.Context(), ns)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("policies of %s (-want +got):\n%s", ns, diff)
	}
}

func TestReplication(t *testing.T) {
	seed := NewSeed()
	ns := replicationNamespace(t)
	as := replicator(t, ns)
	manifest := replicationManifest(ns, "app", v1alpha1.HarborReplicationSpec{Registry: ReplicationRegistry, Repository: sourceApp, Tag: "v1"})

	createReplication(t, manifest, as)
	r := waitForReplication(t, ns, "app")
	replicated := destination(ns, "app") + "/" + sourceApp
	wantArtifacts := []string{replicated + "@" + digest(t, seed.SourceV1)}

	t.Run("status", func(t *testing.T) {
		if e := r.Status.LastExecution; r.UID == "" || r.Status.Destination != destination(ns, "app") || e.Trigger != v1alpha1.ReplicationTriggerManual ||
			e.Succeeded == 0 || e.Failed != 0 || e.StartTime == nil || e.EndTime == nil {
			t.Errorf("uid %q, destination %q, last execution %+v", r.UID, r.Status.Destination, *e)
		}
		wantOwners := []metav1.OwnerReference{{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: "HarborReplication", Name: "app", UID: r.UID}}
		for _, a := range expectArtifacts(t, wantArtifacts, "-n", ns, "-l", v1alpha1.ReplicationLabel+"=app") {
			if diff := cmp.Diff(wantOwners, a.OwnerReferences); diff != "" {
				t.Errorf("%s: owners (-want +got):\n%s", a.Name, diff)
			}
		}
		row := strings.Fields(mustKubectl(t, "-n", ns, "get", "harborreplication", "app", "--no-headers", as))
		if len(row) != 7 || !slices.Equal(row[:6], []string{"app", ReplicationRegistry, sourceApp, "v1", "<none>", "Succeeded"}) {
			t.Errorf("row %q", row)
		}
	})

	t.Run("immutable", func(t *testing.T) {
		if out, err := apply(t, manifest, as); err != nil || !strings.HasSuffix(out, " unchanged") {
			t.Errorf("second apply: %q, %v", out, err)
		}
		v2 := replicationManifest(ns, "app", v1alpha1.HarborReplicationSpec{Registry: ReplicationRegistry, Repository: sourceApp, Tag: "v2"})
		if _, err := apply(t, v2, as); err == nil || !strings.Contains(err.Error(), "spec: Invalid value") || !strings.Contains(err.Error(), "field is immutable") {
			t.Errorf("apply of a new tag: got %v, want an immutable spec", err)
		}
		if _, err := kubectl(t, "-n", ns, "label", "harborreplication", "app", "team=web", as); err == nil || !strings.Contains(err.Error(), "metadata.labels: Invalid value: field is immutable") {
			t.Errorf("label: got %v, want immutable labels", err)
		}
		// These rewrite the annotation that client-side apply keeps, which the server ignores.
		before := mustKubectl(t, "-n", ns, "get", "harborreplication", "app", "-o", "json")
		current := filepath.Join(t.TempDir(), "current.json")
		if err := os.WriteFile(current, []byte(before), 0o600); err != nil {
			t.Fatal(err)
		}
		// kubectl's server-side apply warns if it cannot keep the annotation.
		if out, stderr, err := kubectlWithStderr(t, "apply", "--server-side", "-f", manifestFile(t, manifest), as); err != nil || stderr != "" {
			t.Errorf("server-side apply: %q, %q, %v", out, stderr, err)
		}
		if out, err := kubectl(t, "replace", "-f", current, as); err != nil {
			t.Errorf("replace: %q, %v", out, err)
		}
		if after := mustKubectl(t, "-n", ns, "get", "harborreplication", "app", "-o", "json"); after != before {
			t.Errorf("after server-side apply and replace: %s\nwant %s", after, before)
		}
	})

	t.Run("delete", func(t *testing.T) {
		mustKubectl(t, "-n", ns, "delete", "harborreplication", "app", "--dry-run=server", as)
		mustKubectl(t, "-n", ns, "get", "harborreplication", "app", as)
		expectPolicies(t, ns, policyPrefix(ns)+"app")

		mustKubectl(t, "-n", ns, "delete", "harborreplication", "app", as)
		if _, err := kubectl(t, "-n", ns, "get", "harborreplication", "app", as); err == nil || !strings.Contains(err.Error(), "NotFound") {
			t.Errorf("get after delete: %v", err)
		}
		expectPolicies(t, ns)
		eventuallyKubectl(t, func(out string) error {
			var list v1alpha1.HarborArtifactList
			if err := json.Unmarshal([]byte(out), &list); err != nil {
				return err
			}
			if diff := cmp.Diff(wantArtifacts, locations(list.Items)); diff != "" {
				return fmt.Errorf("artifacts after delete (-want +got):\n%s", diff)
			}
			if a := list.Items[0]; a.Labels[v1alpha1.ReplicationLabel] != "" || len(a.OwnerReferences) > 0 {
				return fmt.Errorf("artifact after delete: labels %v, owners %v", a.Labels, a.OwnerReferences)
			}
			return nil
		}, "-n", ns, "get", "harborartifacts", "-o", "json", "--field-selector", "status.repository="+replicated)
	})
}

func TestServerSideApplyReplication(t *testing.T) {
	ns := replicationNamespace(t)
	as := replicator(t, ns)
	unlabeled := replicationManifest(ns, "app", v1alpha1.HarborReplicationSpec{Registry: ReplicationRegistry, Repository: sourceApp, Tag: "v1"})
	manifest := replicationManifest(ns, "app", v1alpha1.HarborReplicationSpec{Registry: ReplicationRegistry, Repository: sourceApp, Tag: "v1"})
	manifest["metadata"] = map[string]any{"namespace": ns, "name": "app", "labels": map[string]string{"team": "web"}}
	createReplication(t, manifest, "--server-side", as)
	created := waitForReplication(t, ns, "app")

	out, err := apply(t, manifest, "--server-side", "-o", "json", as)
	if err != nil {
		t.Fatal(err)
	}
	var reapplied v1alpha1.HarborReplication
	if err := json.Unmarshal([]byte(out), &reapplied); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(created, reapplied); diff != "" {
		t.Errorf("second apply (-want +got):\n%s", diff)
	}
	if _, err := apply(t, unlabeled, "--server-side", as); err == nil || !strings.Contains(err.Error(), "metadata.labels: Invalid value: field is immutable") {
		t.Errorf("apply without the label: got %v, want immutable labels", err)
	}
	// Client-side apply adds its annotation, which the server ignores.
	if out, err := apply(t, manifest, as); err != nil || !strings.HasSuffix(out, " configured") {
		t.Errorf("client-side apply: %q, %v", out, err)
	}
	var got v1alpha1.HarborReplication
	if err := json.Unmarshal([]byte(mustKubectl(t, "-n", ns, "get", "harborreplication", "app", "-o", "json")), &got); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(created, got); diff != "" {
		t.Errorf("after client-side apply (-want +got):\n%s", diff)
	}

	v2 := replicationManifest(ns, "app", v1alpha1.HarborReplicationSpec{Registry: ReplicationRegistry, Repository: sourceApp, Tag: "v2"})
	if _, err := apply(t, v2, "--server-side", as); err == nil || !strings.Contains(err.Error(), `conflict with "before-first-apply"`) {
		t.Errorf("apply of a new tag: got %v, want a conflict", err)
	}
	if _, err := apply(t, v2, "--server-side", "--force-conflicts", as); err == nil || !strings.Contains(err.Error(), "spec: Invalid value") || !strings.Contains(err.Error(), "field is immutable") {
		t.Errorf("forced apply of a new tag: got %v, want an immutable spec", err)
	}
	if tag := mustKubectl(t, "-n", ns, "get", "harborreplication", "app", "-o", "jsonpath={.spec.tag}"); tag != "v1" {
		t.Errorf("tag %q, want v1", tag)
	}

	// kubectl apply --force deletes and recreates a replication that it cannot patch.
	if _, err := apply(t, v2, "--force", as); err != nil {
		t.Fatal(err)
	}
	if tag := mustKubectl(t, "-n", ns, "get", "harborreplication", "app", "-o", "jsonpath={.spec.tag}"); tag != "v2" {
		t.Errorf("tag %q after a forced apply, want v2", tag)
	}
}

// TestDeleteRunningReplication checks that a delete stops the run, because Harbor refuses to delete the policy of a running replication.
func TestDeleteRunningReplication(t *testing.T) {
	ns := replicationNamespace(t)
	createReplication(t, replicationManifest(ns, "app", v1alpha1.HarborReplicationSpec{Registry: ReplicationRegistry, Repository: sourceApp, Tag: "v1"}))
	phase := mustKubectl(t, "-n", ns, "get", "harborreplication", "app", "-o", "jsonpath={.status.lastExecution.phase}")
	if phase != string(v1alpha1.ReplicationPhaseInProgress) {
		t.Fatalf("phase %q before the delete, want %s", phase, v1alpha1.ReplicationPhaseInProgress)
	}

	start := time.Now()
	mustKubectl(t, "-n", ns, "delete", "harborreplication", "app")
	// Without a stop, Harbor keeps the execution InProgress, and refuses the delete, for up to 30 seconds.
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("delete took %s", d)
	}

	if _, err := kubectl(t, "-n", ns, "get", "harborreplication", "app"); err == nil || !strings.Contains(err.Error(), "NotFound") {
		t.Errorf("get after delete: %v", err)
	}
	expectPolicies(t, ns)
}

// TestSkippedRunDoesNotHideTheRun checks the phase when Harbor skips a run because the previous run is still going.
// Harbor records the skipped run as a newer failed execution.
func TestSkippedRunDoesNotHideTheRun(t *testing.T) {
	ns := replicationNamespace(t)
	createReplication(t, replicationManifest(ns, "app", v1alpha1.HarborReplicationSpec{Registry: ReplicationRegistry, Repository: sourceApp, Tag: "v1"}))
	running := mustKubectl(t, "-n", ns, "get", "harborreplication", "app", "-o", "jsonpath={.status.lastExecution.id}")
	admin := NewAdmin(HarborURL)
	policies, err := admin.replicationPolicies(t.Context(), policyPrefix(ns)+"app")
	if err != nil || len(policies) != 1 {
		t.Fatalf("policies %v, %v", policies, err)
	}
	if err := admin.do(t.Context(), http.MethodPost, "/replication/executions", map[string]int64{"policy_id": policies[0].ID}, nil, http.StatusCreated); err != nil {
		t.Fatal(err)
	}
	var newest []struct {
		StatusText string `json:"status_text"`
	}
	query := url.Values{"policy_id": {strconv.FormatInt(policies[0].ID, 10)}, "sort": {"-id"}, "page_size": {"1"}}
	if err := admin.do(t.Context(), http.MethodGet, "/replication/executions?"+query.Encode(), nil, &newest, http.StatusOK); err != nil {
		t.Fatal(err)
	}
	if len(newest) != 1 || !strings.HasPrefix(newest[0].StatusText, "Execution skipped") {
		t.Fatalf("newest execution %+v, want one that Harbor skipped", newest)
	}

	if id := waitForReplication(t, ns, "app").Status.LastExecution.ID; strconv.FormatInt(id, 10) != running {
		t.Errorf("last execution %d, want %s", id, running)
	}
}

func TestNamespaceDeletionDeletesReplications(t *testing.T) {
	seed := NewSeed()
	ns := replicationNamespace(t)
	const schedule = "0 7 3 * * *"
	createReplication(t, replicationManifest(ns, "every-tag", v1alpha1.HarborReplicationSpec{
		Registry: ReplicationRegistry, Repository: sourceApp, Tag: "v*", Schedule: schedule,
	}))
	if r := waitForReplication(t, ns, "every-tag"); r.Spec.Schedule != schedule {
		t.Errorf("schedule %q, want %q", r.Spec.Schedule, schedule)
	}
	replicated := destination(ns, "every-tag") + "/" + sourceApp
	want := []string{replicated + "@" + digest(t, seed.SourceV1), replicated + "@" + digest(t, seed.SourceV2)}
	slices.Sort(want)
	expectArtifacts(t, want, "-n", ns, "-l", v1alpha1.ReplicationLabel+"=every-tag")

	mustKubectl(t, "delete", "namespace", ns, "--wait=false")

	eventuallyWithin(t, replicationTimeout, func() error {
		if names, err := policyNames(t.Context(), ns); err != nil || len(names) > 0 {
			return fmt.Errorf("policies of %s: %v, %v", ns, names, err)
		}
		if _, err := kubectl(t, "get", "namespace", ns); err == nil || !strings.Contains(err.Error(), "NotFound") {
			return fmt.Errorf("namespace %s is not deleted: %v", ns, err)
		}
		return nil
	})
}

func TestReplicationRejections(t *testing.T) {
	labeled := namespaceWithServiceAccounts(t)
	label(t, labeled, HarborProject)
	deleteReplicationsAtEnd(t, labeled)
	unlabeled := newNamespace(t)
	deleteReplicationsAtEnd(t, unlabeled)
	viewer := "--as=system:serviceaccount:" + labeled + ":viewer"

	valid := v1alpha1.HarborReplicationSpec{Registry: ReplicationRegistry, Repository: sourceApp, Tag: "v1"}
	unlisted, everyFiveMinutes, february30, badPattern, slowPattern := valid, valid, valid, valid, valid
	unlisted.Registry = unlistedRegistry
	everyFiveMinutes.Schedule = "0 */5 * * * *"
	february30.Schedule = "0 0 0 30 2 *"
	badPattern.Tag = "v["
	slowPattern.Tag = "*?*?*Z"
	for _, tc := range []struct {
		name, namespace string
		spec            v1alpha1.HarborReplicationSpec
		args            []string
		want            string
	}{
		{"registry not on the allow-list", labeled, unlisted, nil, `spec.registry: Unsupported value: "e2e-unlisted"`},
		{"schedule more often than hourly", labeled, everyFiveMinutes, nil, `spec.schedule: Invalid value: "0 */5 * * * *"`},
		{"schedule that never runs", labeled, february30, nil, `spec.schedule: Invalid value: "0 0 0 30 2 *": must match a date that exists`},
		{"tag that is not a pattern", labeled, badPattern, nil, `spec.tag: Invalid value: "v["`},
		{"tag pattern that Harbor matches slowly", labeled, slowPattern, nil, `spec.tag: Invalid value: "*?*?*Z": must have at most two *`},
		{"unlabeled namespace", unlabeled, valid, nil, "namespace " + unlabeled + " does not exist, or is not labeled"},
		{"viewer", labeled, valid, []string{viewer}, `cannot create resource "harborreplications"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := apply(t, replicationManifest(tc.namespace, "rejected", tc.spec), tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
	expectPolicies(t, labeled)
	expectPolicies(t, unlabeled)
	if out := mustKubectl(t, "-n", labeled, "get", "harborreplications", viewer); out != "" {
		t.Errorf("viewer list: %q", out)
	}
}
