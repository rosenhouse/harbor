package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	restclient "k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apiserver"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/namespaces"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/registry"
)

// fakeHarbor serves repository app, or fails with err. Its lists wait until release is closed.
type fakeHarbor struct {
	err     error
	release chan struct{}
}

func (h fakeHarbor) ListRepositories(ctx context.Context, _ string) ([]harbor.Repository, error) {
	if h.release != nil {
		select {
		case <-h.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []harbor.Repository{{ID: 1, Name: "proj/app"}}, h.err
}

func (h fakeHarbor) ListArtifacts(context.Context, string, string) ([]harbor.Artifact, error) {
	return nil, h.err
}

func namespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

var testHarborOptions = harborOptions{Project: "proj", Timeout: time.Minute, PollInterval: 10 * time.Millisecond, StalenessLimit: time.Minute}

// startServer runs the post-start hooks of a server for project "proj" and returns its handler.
func startServer(t *testing.T, kube *fake.Clientset, h registry.Harbor) http.Handler {
	t.Helper()
	return startServerWithOptions(t, kube, h, testHarborOptions)
}

func startServerWithOptions(t *testing.T, kube *fake.Clientset, h registry.Harbor, o harborOptions) http.Handler {
	t.Helper()
	return startServerWithReplications(t, kube, h, o, nil, replicationOptions{})
}

func startServerWithReplications(t *testing.T, kube *fake.Clientset, h registry.Harbor, o harborOptions, rh registry.ReplicationHarbor, r replicationOptions) http.Handler {
	t.Helper()
	c := apiserver.NewConfig()
	c.ExternalAddress = "localhost:443"
	c.LoopbackClientConfig = &restclient.Config{}
	s, err := newServer(c.Complete(nil), kube, h, &o, rh, &r)
	if err != nil {
		t.Fatal(err)
	}
	handler := s.PrepareRun().Handler
	s.RunPostStartHooks(t.Context())
	return handler
}

func serve(h http.Handler, path string) (int, string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func waitForOK(t *testing.T, h http.Handler, path string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		code, body := serve(h, path)
		if code == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s returned %d:\n%s", path, code, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServerIsReadyOnceNamespacesSync(t *testing.T) {
	kube := fake.NewClientset()
	releaseList := make(chan struct{})
	kube.PrependReactor("list", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
		<-releaseList
		return false, nil, nil
	})
	h := startServer(t, kube, fakeHarbor{})

	if code, body := serve(h, "/readyz?verbose"); code != http.StatusInternalServerError || !strings.Contains(body, "[-]namespaces-synced failed") {
		t.Errorf("before listing namespaces: /readyz returned %d:\n%s", code, body)
	}
	waitForOK(t, h, "/livez?verbose")

	close(releaseList)
	waitForOK(t, h, "/readyz?verbose")
}

func TestServerIsReadyOnceItsFirstPollEnds(t *testing.T) {
	for _, err := range []error{nil, harbor.ErrUnavailable} {
		kube := fake.NewClientset(namespace("labeled", map[string]string{namespaces.ProjectLabel: "proj"}))
		release := make(chan struct{})
		o := testHarborOptions
		// Readiness waits for the first poll, even long past the timeout.
		o.Timeout = time.Millisecond
		h := startServerWithOptions(t, kube, fakeHarbor{err: err, release: release}, o)

		time.Sleep(100 * time.Millisecond)
		if code, body := serve(h, "/readyz?verbose"); code != http.StatusInternalServerError || !strings.Contains(body, "[-]harbor-read failed") {
			t.Errorf("%v: during the first poll: /readyz returned %d:\n%s", err, code, body)
		}
		close(release)
		waitForOK(t, h, "/readyz?verbose")
	}
}

func TestReadinessDoesNotDependOnHarbor(t *testing.T) {
	kube := fake.NewClientset(namespace("labeled", map[string]string{namespaces.ProjectLabel: "proj"}))
	h := startServer(t, kube, fakeHarbor{err: harbor.ErrUnavailable})
	waitForOK(t, h, "/readyz")
	if code, body := serve(h, "/apis/harbor.goharbor.io/v1alpha1/namespaces/labeled/harborrepositories"); code != http.StatusServiceUnavailable {
		t.Errorf("list returned %d: %s", code, body)
	}
}

func TestServerWatchesOnlyLabeledNamespaces(t *testing.T) {
	kube := fake.NewClientset(
		namespace("labeled", map[string]string{namespaces.ProjectLabel: "proj"}),
		namespace("other-project", map[string]string{namespaces.ProjectLabel: "other"}),
		namespace("unlabeled", nil),
	)
	h := startServer(t, kube, fakeHarbor{})
	waitForOK(t, h, "/readyz")

	code, body := serve(h, "/apis/harbor.goharbor.io/v1alpha1/harborrepositories")
	var list v1alpha1.HarborRepositoryList
	if err := json.Unmarshal([]byte(body), &list); code != http.StatusOK || err != nil {
		t.Fatalf("list returned %d, %v: %s", code, err, body)
	}
	var visible []string
	for _, r := range list.Items {
		visible = append(visible, r.Namespace+"/"+r.Name)
	}
	if diff := cmp.Diff([]string{"labeled/app"}, visible); diff != "" {
		t.Errorf("repositories (-want +got):\n%s", diff)
	}

	for _, a := range kube.Actions() {
		if l, ok := a.(clienttesting.ListAction); ok && l.GetListRestrictions().Labels.String() != namespaces.ProjectLabel {
			t.Errorf("listed namespaces matching %q, want only those with the %s label", l.GetListRestrictions().Labels, namespaces.ProjectLabel)
		}
		if w, ok := a.(clienttesting.WatchAction); ok && w.GetWatchRestrictions().Labels.String() != namespaces.ProjectLabel {
			t.Errorf("watched namespaces matching %q, want only those with the %s label", w.GetWatchRestrictions().Labels, namespaces.ProjectLabel)
		}
	}
}

// replicatedHarbor has an artifact that replication nginx in namespace labeled copied.
type replicatedHarbor struct{}

func (replicatedHarbor) ListRepositories(context.Context, string) ([]harbor.Repository, error) {
	return []harbor.Repository{{ID: 1, Name: "proj/k8s/labeled/nginx/library/nginx"}}, nil
}

func (replicatedHarbor) ListArtifacts(context.Context, string, string) ([]harbor.Artifact, error) {
	return []harbor.Artifact{{ID: 1, Digest: "sha256:" + strings.Repeat("0", 64), Type: "IMAGE"}}, nil
}

// fakeReplicationHarbor has the policy of replication nginx in namespace labeled, and counts its calls.
type fakeReplicationHarbor struct {
	registry.ReplicationHarbor
	calls atomic.Int32
}

func (f *fakeReplicationHarbor) ListReplicationPolicies(context.Context, string) ([]harbor.ReplicationPolicy, error) {
	f.calls.Add(1)
	return []harbor.ReplicationPolicy{{
		ID:   1,
		Name: "k8s.proj.labeled.nginx",
		Description: `{"managedBy":"harbor-apiserver","namespace":"labeled","namespaceUID":"labeled-uid","name":"nginx","uid":"nginx-uid",` +
			`"spec":{"registry":"docker-hub","repository":"library/nginx","tag":"1.27"}}`,
		SrcRegistry:               &harbor.Registry{ID: 2, Name: "docker-hub"},
		DestNamespace:             "proj/k8s/labeled/nginx",
		DestNamespaceReplaceCount: ptr.To[int8](0),
		Trigger:                   &harbor.ReplicationTrigger{Type: "manual"},
		Filters:                   []harbor.ReplicationFilter{{Type: "name", Value: "library/nginx"}, {Type: "tag", Value: "1.27", Decoration: "matches"}},
	}}, nil
}

func (f *fakeReplicationHarbor) LatestReplicationExecution(context.Context, int64) (*harbor.ReplicationExecution, error) {
	f.calls.Add(1)
	return nil, nil
}

var enabledReplications = replicationOptions{Enabled: true, Registries: []string{"docker-hub"}, Prefix: "k8s"}

func labeledNamespace() *corev1.Namespace {
	ns := namespace("labeled", map[string]string{namespaces.ProjectLabel: "proj"})
	ns.UID = "labeled-uid"
	return ns
}

func TestServerServesReplicationsOnlyWhenEnabled(t *testing.T) {
	for _, r := range []replicationOptions{{}, enabledReplications} {
		rh := &fakeReplicationHarbor{}
		h := startServerWithReplications(t, fake.NewClientset(labeledNamespace()), replicatedHarbor{}, testHarborOptions, rh, r)
		waitForOK(t, h, "/readyz")

		code, body := serve(h, "/apis/harbor.goharbor.io/v1alpha1/namespaces/labeled/harborreplications/nginx")
		if r.Enabled {
			if code != http.StatusOK || !strings.Contains(body, `"uid":"nginx-uid"`) {
				t.Errorf("enabled: get returned %d: %s", code, body)
			}
		} else if code != http.StatusNotFound || rh.calls.Load() != 0 {
			t.Errorf("disabled: get returned %d, and Harbor got %d replication calls: %s", code, rh.calls.Load(), body)
		}
	}
}

func TestServerLinksReplicatedArtifactsInItsFirstPoll(t *testing.T) {
	kube := fake.NewClientset(labeledNamespace())
	releaseList := make(chan struct{})
	kube.PrependReactor("list", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
		<-releaseList
		return false, nil, nil
	})
	o := testHarborOptions
	o.PollInterval = time.Hour
	h := startServerWithReplications(t, kube, replicatedHarbor{}, o, &fakeReplicationHarbor{}, enabledReplications)

	// A server that polls before it lists namespaces would poll now, and link nothing until the next poll.
	time.Sleep(100 * time.Millisecond)
	close(releaseList)
	waitForOK(t, h, "/readyz")

	code, body := serve(h, "/apis/harbor.goharbor.io/v1alpha1/namespaces/labeled/harborartifacts?labelSelector="+v1alpha1.ReplicationLabel+"%3Dnginx")
	var list v1alpha1.HarborArtifactList
	if err := json.Unmarshal([]byte(body), &list); code != http.StatusOK || err != nil {
		t.Fatalf("list returned %d, %v: %s", code, err, body)
	}
	if len(list.Items) != 1 || len(list.Items[0].OwnerReferences) != 1 || list.Items[0].OwnerReferences[0].UID != "nginx-uid" {
		t.Errorf("artifacts %+v", list.Items)
	}
}
