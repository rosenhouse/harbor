package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

func newReplications() (*Replications, *fakeReplicationHarbor, namespaceObjects) {
	h := &fakeReplicationHarbor{registries: []harbor.Registry{
		{ID: 3, Name: "hub", Type: "docker-hub"},
		{ID: 5, Name: "quay", Type: "quay"},
		{ID: 9, Name: "other", Type: "harbor"},
	}}
	n := namespaceObjects{"ns1": namespace("ns1"), "ns2": namespace("ns2")}
	r := NewReplications(h, n, replicationConfig)
	r.retryInterval, r.stopWait = time.Millisecond, 50*time.Millisecond
	return r, h, n
}

func replication(name string) *v1alpha1.HarborReplication {
	return &v1alpha1.HarborReplication{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.HarborReplicationSpec{Registry: "hub", Repository: "library/" + name, Tag: "1.27*"},
	}
}

func create(t *testing.T, r *Replications, namespace string, obj *v1alpha1.HarborReplication) *v1alpha1.HarborReplication {
	t.Helper()
	out, err := r.Create(inNamespace(namespace), obj, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return out.(*v1alpha1.HarborReplication)
}

func getReplication(r *Replications, namespace, name string) (*v1alpha1.HarborReplication, error) {
	obj, err := r.Get(inNamespace(namespace), name, &metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return obj.(*v1alpha1.HarborReplication), nil
}

func listReplications(t *testing.T, r *Replications, namespace string, opts *metainternalversion.ListOptions) []string {
	t.Helper()
	obj, err := r.List(inNamespace(namespace), opts)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, o := range obj.(*v1alpha1.HarborReplicationList).Items {
		names = append(names, o.Namespace+"/"+o.Name)
	}
	return names
}

func TestCreateReplication(t *testing.T) {
	r, h, _ := newReplications()
	obj := replication("nginx")
	obj.UID = "chosen"
	obj.CreationTimestamp = metav1.NewTime(updated)
	obj.Labels = map[string]string{"app": "web"}
	out := create(t, r, "ns1", obj)

	if diff := cmp.Diff([]string{"CreateReplicationPolicy k8s.proj.ns1.nginx", "StartReplication 1"}, h.writes()); diff != "" {
		t.Errorf("writes (-want +got):\n%s", diff)
	}
	p, _ := h.policy("k8s.proj.ns1.nginx")
	if p.SrcRegistry.ID != 3 {
		t.Errorf("source registry %d, want 3", p.SrcRegistry.ID)
	}
	if out.UID == "" || out.UID == "chosen" {
		t.Errorf("UID %q, want a new one", out.UID)
	}
	want := &v1alpha1.HarborReplication{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "nginx",
			Namespace:         "ns1",
			UID:               out.UID,
			Generation:        1,
			CreationTimestamp: metav1.NewTime(created),
			Labels:            map[string]string{"app": "web"},
		},
		Spec: obj.Spec,
		Status: v1alpha1.HarborReplicationStatus{
			Destination: "proj/k8s/ns1/nginx",
			LastExecution: &v1alpha1.HarborReplicationExecution{
				ID: 2, Trigger: v1alpha1.ReplicationTriggerManual, Phase: v1alpha1.ReplicationPhaseInProgress,
				StartTime: &metav1.Time{Time: created}, Total: 1, InProgress: 1,
			},
		},
	}
	want.ResourceVersion = out.ResourceVersion
	if diff := cmp.Diff(want, out); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}

	got, err := getReplication(r, "ns1", "nginx")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(out, got); diff != "" {
		t.Errorf("get after create (-want +got):\n%s", diff)
	}
}

func TestCreateReplicationPreparesObjectForValidation(t *testing.T) {
	r, _, _ := newReplications()
	obj := replication("nginx")
	obj.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl"}}
	obj.Status.Destination = "elsewhere"
	var validated *v1alpha1.HarborReplication
	validate := func(_ context.Context, o runtime.Object) error {
		validated = o.(*v1alpha1.HarborReplication)
		return nil
	}
	if _, err := r.Create(inNamespace("ns1"), obj, validate, &metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if validated.Namespace != "ns1" || validated.UID == "" || validated.CreationTimestamp.IsZero() {
		t.Errorf("validated metadata %+v, want namespace, UID, and creation time", validated.ObjectMeta)
	}
	if validated.ManagedFields != nil {
		t.Errorf("validated managed fields %+v", validated.ManagedFields)
	}
	if validated.Status != (v1alpha1.HarborReplicationStatus{}) {
		t.Errorf("validated status %+v", validated.Status)
	}
}

func TestCreateReplicationWithGeneratedName(t *testing.T) {
	r, _, _ := newReplications()
	obj := replication("web")
	obj.Name, obj.GenerateName = "", "web-"
	out := create(t, r, "ns1", obj)
	if !strings.HasPrefix(out.Name, "web-") || len(out.Name) != len("web-")+5 {
		t.Errorf("name %q, want web- and 5 characters", out.Name)
	}
	if _, err := getReplication(r, "ns1", out.Name); err != nil {
		t.Error(err)
	}
}

// sequence generates names with the suffixes a, b, c, and so on.
type sequence struct{ n int }

func (s *sequence) GenerateName(base string) string {
	s.n++
	return base + string(rune('a'+s.n-1))
}

func TestCreateReplicationRetriesGeneratedNames(t *testing.T) {
	r, _, _ := newReplications()
	create(t, r, "ns1", replication("web-a"))
	create(t, r, "ns1", replication("web-b"))
	r.strategy.NameGenerator = &sequence{}
	obj := replication("web")
	obj.Name, obj.GenerateName = "", "web-"
	if out := create(t, r, "ns1", obj); out.Name != "web-c" {
		t.Errorf("name %q, want web-c", out.Name)
	}

	for _, suffix := range "defghijk" {
		create(t, r, "ns1", replication("web-"+string(suffix)))
	}
	r.strategy.NameGenerator = &sequence{}
	_, err := r.Create(inNamespace("ns1"), obj, nil, &metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) || !strings.Contains(err.Error(), `"web-h" already exists, the server was not able to generate a unique name`) {
		t.Errorf("got %v, want a generated name conflict after 8 names", err)
	}
}

// createAgain is a create, and a dry-run create, of a replication whose policy exists, with the writes that each makes.
var createAgain = map[string]struct {
	options *metav1.CreateOptions
	writes  []string
}{
	"create":  {&metav1.CreateOptions{}, []string{"CreateReplicationPolicy k8s.proj.ns1.nginx"}},
	"dry run": {&metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}, nil},
}

func TestCreateExistingReplication(t *testing.T) {
	for name, tc := range createAgain {
		t.Run(name, func(t *testing.T) {
			r, h, _ := newReplications()
			create(t, r, "ns1", replication("nginx"))
			h.calls = nil
			_, err := r.Create(inNamespace("ns1"), replication("nginx"), nil, tc.options)
			if !apierrors.IsAlreadyExists(err) || err.Error() != `harborreplications.harbor.goharbor.io "nginx" already exists` {
				t.Errorf("got %v, want AlreadyExists", err)
			}
			if diff := cmp.Diff(tc.writes, h.writes()); diff != "" {
				t.Errorf("writes (-want +got):\n%s", diff)
			}
			create(t, r, "ns2", replication("nginx"))
		})
	}
}

func TestCreateReplicationOverHiddenPolicy(t *testing.T) {
	for name, tc := range createAgain {
		t.Run(name, func(t *testing.T) {
			r, h, n := newReplications()
			create(t, r, "ns1", replication("nginx"))
			n["ns1"] = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns1", UID: "recreated"}}
			h.calls = nil
			_, err := r.Create(inNamespace("ns1"), replication("nginx"), nil, tc.options)
			if !apierrors.IsAlreadyExists(err) || !strings.Contains(err.Error(), "policy k8s.proj.ns1.nginx") || !strings.Contains(err.Error(), "A Harbor administrator must delete it") {
				t.Errorf("got %v, want AlreadyExists that names the hidden policy", err)
			}
			if diff := cmp.Diff(tc.writes, h.writes()); diff != "" {
				t.Errorf("writes (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCreateInvalidReplication(t *testing.T) {
	r, h, _ := newReplications()
	obj := replication("nginx")
	obj.Spec.Registry = "other"
	_, err := r.Create(inNamespace("ns1"), obj, nil, &metav1.CreateOptions{})
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), `spec.registry: Unsupported value: "other": supported values: "hub", "quay", "missing"`) {
		t.Errorf("got %v, want Invalid", err)
	}
	if len(h.calls) != 0 {
		t.Errorf("calls %v, want none", h.calls)
	}
}

func TestValidateReplication(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*v1alpha1.HarborReplication)
		want   []string
	}{
		"valid":                   {change: func(*v1alpha1.HarborReplication) {}},
		"schedule":                {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 5 3 * * 1-5" }},
		"any tag":                 {change: func(o *v1alpha1.HarborReplication) { o.Spec.Tag = "*" }},
		"separators":              {change: func(o *v1alpha1.HarborReplication) { o.Spec.Repository = "a0/b.c/d_e/f__g/h---i" }},
		"one path segment":        {change: func(o *v1alpha1.HarborReplication) { o.Spec.Repository = "nginx" }},
		"uppercase name":          {change: func(o *v1alpha1.HarborReplication) { o.Name = "Nginx" }, want: []string{"metadata.name"}},
		"dotted name":             {change: func(o *v1alpha1.HarborReplication) { o.Name = "a.b" }, want: []string{"metadata.name"}},
		"long name":               {change: func(o *v1alpha1.HarborReplication) { o.Name = strings.Repeat("a", 64) }, want: []string{"metadata.name"}},
		"finalizers":              {change: func(o *v1alpha1.HarborReplication) { o.Finalizers = []string{"x"} }, want: []string{"metadata.finalizers"}},
		"owner references":        {change: func(o *v1alpha1.HarborReplication) { o.OwnerReferences = []metav1.OwnerReference{{Name: "x"}} }, want: []string{"metadata.ownerReferences"}},
		"resource version":        {change: func(o *v1alpha1.HarborReplication) { o.ResourceVersion = "1" }, want: []string{"metadata.resourceVersion"}},
		"registry not allowed":    {change: func(o *v1alpha1.HarborReplication) { o.Spec.Registry = "other" }, want: []string{"spec.registry"}},
		"no registry":             {change: func(o *v1alpha1.HarborReplication) { o.Spec.Registry = "" }, want: []string{"spec.registry"}},
		"no repository":           {change: func(o *v1alpha1.HarborReplication) { o.Spec.Repository = "" }, want: []string{"spec.repository"}},
		"repository glob":         {change: func(o *v1alpha1.HarborReplication) { o.Spec.Repository = "library/*" }, want: []string{"spec.repository"}},
		"repository alternatives": {change: func(o *v1alpha1.HarborReplication) { o.Spec.Repository = "library/{a,b}" }, want: []string{"spec.repository"}},
		"uppercase repository":    {change: func(o *v1alpha1.HarborReplication) { o.Spec.Repository = "Library/nginx" }, want: []string{"spec.repository"}},
		"leading slash":           {change: func(o *v1alpha1.HarborReplication) { o.Spec.Repository = "/nginx" }, want: []string{"spec.repository"}},
		"trailing slash":          {change: func(o *v1alpha1.HarborReplication) { o.Spec.Repository = "nginx/" }, want: []string{"spec.repository"}},
		"empty segment":           {change: func(o *v1alpha1.HarborReplication) { o.Spec.Repository = "a//b" }, want: []string{"spec.repository"}},
		"registry host":           {change: func(o *v1alpha1.HarborReplication) { o.Spec.Repository = "docker.io:443/nginx" }, want: []string{"spec.repository"}},
		"long destination": {change: func(o *v1alpha1.HarborReplication) {
			o.Spec.Repository = strings.Repeat("a", 255-len("proj/k8s/ns1/nginx/")+1)
		}, want: []string{"spec.repository"}},
		"no tag":                       {change: func(o *v1alpha1.HarborReplication) { o.Spec.Tag = "" }, want: []string{"spec.tag"}},
		"long tag":                     {change: func(o *v1alpha1.HarborReplication) { o.Spec.Tag = strings.Repeat("a", 129) }, want: []string{"spec.tag"}},
		"five schedule fields":         {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 3 * * *" }, want: []string{"spec.schedule"}},
		"seven schedule fields":        {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 0 3 * * * 2026" }, want: []string{"spec.schedule"}},
		"schedule time zone":           {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "TZ=UTC 0 0 3 * * *" }, want: []string{"spec.schedule"}},
		"schedule descriptor":          {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "@hourly" }, want: []string{"spec.schedule"}},
		"double space in schedule":     {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0  0 3 * * *" }, want: []string{"spec.schedule"}},
		"trailing space in schedule":   {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 0 3 * * * " }, want: []string{"spec.schedule"}},
		"schedule out of range":        {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 0 24 * * *" }, want: []string{"spec.schedule"}},
		"schedule minute out of range": {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 60 3 * * *" }, want: []string{"spec.schedule"}},
		"schedule seconds":             {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "30 0 3 * * *" }, want: []string{"spec.schedule"}},
		"schedule every minute":        {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 * 3 * * *" }, want: []string{"spec.schedule"}},
		"schedule minute step":         {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 */5 3 * * *" }, want: []string{"spec.schedule"}},
		"schedule minute list":         {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 0,30 3 * * *" }, want: []string{"spec.schedule"}},
		"schedule minute range":        {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 1-2 3 * * *" }, want: []string{"spec.schedule"}},
		"schedule minute with a sign":  {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 +5 3 * * *" }, want: []string{"spec.schedule"}},
		"schedule that fits in length": {change: func(o *v1alpha1.HarborReplication) {
			o.Spec.Schedule = "0 5 0,2,4,6,8,10,12,14,16,18,20,22 1,8,15,22 1,2,3,4,5,6,7,8,9 *"
		}},
		"long schedule": {change: func(o *v1alpha1.HarborReplication) {
			o.Spec.Schedule = "0 5 0,2,4,6,8,10,12,14,16,18,20,22 1,8,15,22 1,2,3,4,5,6,7,8,10 *"
		}, want: []string{"spec.schedule"}},
		"several": {change: func(o *v1alpha1.HarborReplication) { o.Spec.Tag, o.Finalizers = "", []string{"x"} }, want: []string{"metadata.finalizers", "spec.tag"}},
		"destination that fits in length": {change: func(o *v1alpha1.HarborReplication) {
			o.Spec.Repository = strings.Repeat("a", 255-len("proj/k8s/ns1/nginx/"))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			obj := replication("nginx")
			obj.Namespace = "ns1"
			tc.change(obj)
			var got []string
			for _, err := range replicationConfig.validate(obj) {
				got = append(got, err.Field)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("invalid fields (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCreateReplicationInNamespaceWithoutProject(t *testing.T) {
	r, h, _ := newReplications()
	_, err := r.Create(inNamespace("ns3"), replication("nginx"), nil, &metav1.CreateOptions{})
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "namespace ns3 is not labeled") {
		t.Errorf("got %v, want Forbidden", err)
	}
	if len(h.calls) != 0 {
		t.Errorf("calls %v, want none", h.calls)
	}
}

func TestCreateReplicationInTerminatingNamespace(t *testing.T) {
	for name, terminate := range map[string]func(*corev1.Namespace){
		"phase":              func(ns *corev1.Namespace) { ns.Status.Phase = corev1.NamespaceTerminating },
		"deletion timestamp": func(ns *corev1.Namespace) { ns.DeletionTimestamp = &metav1.Time{Time: created} },
	} {
		t.Run(name, func(t *testing.T) {
			r, h, n := newReplications()
			terminate(n["ns1"])
			_, err := r.Create(inNamespace("ns1"), replication("nginx"), nil, &metav1.CreateOptions{})
			if !apierrors.IsForbidden(err) || !apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
				t.Errorf("got %v, want Forbidden because the namespace is terminating", err)
			}
			if len(h.calls) != 0 {
				t.Errorf("calls %v, want none", h.calls)
			}
		})
	}
}

func TestCreateReplicationThatAdmissionRejects(t *testing.T) {
	r, h, _ := newReplications()
	rejected := apierrors.NewForbidden(replicationsResource, "nginx", errors.New("no"))
	_, err := r.Create(inNamespace("ns1"), replication("nginx"), func(context.Context, runtime.Object) error { return rejected }, &metav1.CreateOptions{})
	if err != rejected {
		t.Errorf("got %v, want %v", err, rejected)
	}
	if len(h.calls) != 0 {
		t.Errorf("calls %v, want none", h.calls)
	}
}

func TestCreateReplicationFromMissingRegistry(t *testing.T) {
	r, h, _ := newReplications()
	obj := replication("nginx")
	obj.Spec.Registry = "missing"
	_, err := r.Create(inNamespace("ns1"), obj, nil, &metav1.CreateOptions{})
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), `spec.registry: Not found: "missing"`) {
		t.Errorf("got %v, want Invalid", err)
	}
	if w := h.writes(); len(w) != 0 {
		t.Errorf("writes %v, want none", w)
	}
}

func TestCreateReplicationDryRun(t *testing.T) {
	r, h, _ := newReplications()
	obj, err := r.Create(inNamespace("ns1"), replication("nginx"), nil, &metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		t.Fatal(err)
	}
	if w := h.writes(); len(w) != 0 {
		t.Errorf("writes %v, want none", w)
	}
	out := obj.(*v1alpha1.HarborReplication)
	if out.Name != "nginx" || out.UID == "" || out.CreationTimestamp.IsZero() || out.ResourceVersion == "" {
		t.Errorf("metadata %+v, want name, UID, creation time, and resourceVersion", out.ObjectMeta)
	}
	if want := (v1alpha1.HarborReplicationStatus{Destination: "proj/k8s/ns1/nginx"}); out.Status != want {
		t.Errorf("status %+v, want %+v", out.Status, want)
	}

	missing := replication("nginx")
	missing.Spec.Registry = "missing"
	if _, err := r.Create(inNamespace("ns1"), missing, nil, &metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}); !apierrors.IsInvalid(err) {
		t.Errorf("got %v, want Invalid", err)
	}

	h.errs = map[string]error{"ListReplicationPolicies": harbor.ErrUnavailable}
	if _, err := r.Create(inNamespace("ns1"), replication("nginx"), nil, &metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}); !apierrors.IsServiceUnavailable(err) {
		t.Errorf("when policies cannot be listed: got %v, want ServiceUnavailable", err)
	}
}

func TestCreateReplicationThatHarborStoresDifferently(t *testing.T) {
	recreateNamespace := func(h *fakeReplicationHarbor, n namespaceObjects) {
		h.onCreate = func() { n["ns1"] = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns1", UID: "recreated"}} }
	}
	beforeHarbor23 := func(h *fakeReplicationHarbor, _ namespaceObjects) { h.beforeHarbor23 = true }
	const undeleted = "The server could not delete Harbor replication policy k8s.proj.ns1.nginx, so a Harbor administrator must delete it"
	for name, tc := range map[string]struct {
		change    func(*fakeReplicationHarbor, namespaceObjects)
		deleteErr error
		want      string
	}{
		"hidden":                      {recreateNamespace, nil, "harbor stored a replication policy that the server does not show, so the server deleted the policy"},
		"hidden and undeleted":        {recreateNamespace, harbor.ErrUnavailable, "harbor stored a replication policy that the server does not show. " + undeleted},
		"before harbor 2.3":           {beforeHarbor23, nil, "replications require harbor 2.3 or later, so the server deleted the policy"},
		"undeleted before harbor 2.3": {beforeHarbor23, harbor.ErrUnavailable, "replications require harbor 2.3 or later. " + undeleted},
	} {
		t.Run(name, func(t *testing.T) {
			r, h, n := newReplications()
			tc.change(h, n)
			h.errs = map[string]error{"DeleteReplicationPolicy": tc.deleteErr}
			_, err := r.Create(inNamespace("ns1"), replication("nginx"), nil, &metav1.CreateOptions{})
			if want := "Internal error occurred: " + tc.want; !apierrors.IsInternalError(err) || err.Error() != want {
				t.Errorf("got %v, want an internal error: %s", err, want)
			}
			if diff := cmp.Diff([]string{"CreateReplicationPolicy k8s.proj.ns1.nginx", "DeleteReplicationPolicy 1"}, h.writes()); diff != "" {
				t.Errorf("writes (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCreateReplicationThatDoesNotStart(t *testing.T) {
	for _, method := range []string{"StartReplication", "LatestReplicationExecution"} {
		t.Run(method, func(t *testing.T) {
			r, h, _ := newReplications()
			h.errs = map[string]error{method: harbor.ErrUnavailable}
			out := create(t, r, "ns1", replication("nginx"))
			if out.Status.LastExecution != nil {
				t.Errorf("last execution %+v, want none", out.Status.LastExecution)
			}
			if _, ok := h.policy("k8s.proj.ns1.nginx"); !ok {
				t.Error("the policy is gone")
			}
		})
	}
}

func TestCreateOtherKind(t *testing.T) {
	r, _, _ := newReplications()
	if _, err := r.Create(inNamespace("ns1"), &v1alpha1.HarborArtifact{}, nil, &metav1.CreateOptions{}); !apierrors.IsBadRequest(err) {
		t.Errorf("got %v, want BadRequest", err)
	}
}

// errLeak is an error that holds what responses must not include.
var errLeak = errors.New("https://harbor.example.com/api/v2.0: secret")

func TestReplicationHarborErrors(t *testing.T) {
	for _, tc := range []struct {
		method string
		err    error
		code   int
		msg    string
	}{
		{"ListRegistries", harbor.ErrUnavailable, http.StatusServiceUnavailable, "harbor is unavailable"},
		{"ListRegistries", fmt.Errorf("%w: %w", harbor.ErrUnauthorized, errLeak), http.StatusInternalServerError, "harbor rejected the robot account credentials"},
		{"ListRegistries", fmt.Errorf("%w: %w", harbor.ErrForbidden, errLeak), http.StatusInternalServerError, "harbor denied the robot account access"},
		{"CreateReplicationPolicy", fmt.Errorf("%w: %w", harbor.ErrBadRequest, errLeak), http.StatusBadRequest, "harbor rejected the replication policy"},
		{"CreateReplicationPolicy", errLeak, http.StatusInternalServerError, "unexpected error from harbor"},
		{"CreateReplicationPolicy", fmt.Errorf("%w: %w", harbor.ErrConflict, errLeak), http.StatusConflict, `"nginx" already exists`},
		{"GetReplicationPolicy", fmt.Errorf("%w: %w", harbor.ErrUnavailable, errLeak), http.StatusServiceUnavailable, "harbor is unavailable"},
	} {
		t.Run(tc.method+" "+tc.msg, func(t *testing.T) {
			r, h, _ := newReplications()
			h.errs = map[string]error{tc.method: tc.err}
			_, err := r.Create(inNamespace("ns1"), replication("nginx"), nil, &metav1.CreateOptions{})
			checkHarborError(t, err, tc.code, tc.msg)
		})
	}
}

func checkHarborError(t *testing.T, err error, code int, msg string) {
	t.Helper()
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		t.Fatalf("got %v, want an API status", err)
	}
	if got := status.Status(); got.Code != int32(code) || !strings.Contains(got.Message, msg) {
		t.Errorf("got %d %q, want %d with %q", got.Code, got.Message, code, msg)
	}
	if strings.Contains(fmt.Sprintf("%+v", status.Status()), "secret") {
		t.Errorf("%+v includes Harbor's response", status.Status())
	}
}

func TestGetReplication(t *testing.T) {
	r, h, n := newReplications()
	create(t, r, "ns1", replication("nginx"))
	h.finish("Succeed")
	got, err := getReplication(r, "ns1", "nginx")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.LastExecution.Phase != v1alpha1.ReplicationPhaseSucceeded {
		t.Errorf("phase %q, want Succeeded", got.Status.LastExecution.Phase)
	}

	for _, tc := range []struct{ namespace, name string }{{"ns2", "nginx"}, {"ns1", "nginx2"}, {"ns1", "ngin"}, {"ns3", "nginx"}} {
		if _, err := getReplication(r, tc.namespace, tc.name); !apierrors.IsNotFound(err) {
			t.Errorf("%s/%s: got %v, want NotFound", tc.namespace, tc.name, err)
		}
	}

	n["ns1"] = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns1", UID: "recreated"}}
	if _, err := getReplication(r, "ns1", "nginx"); !apierrors.IsNotFound(err) {
		t.Errorf("in a recreated namespace: got %v, want NotFound", err)
	}
}

func TestGetReplicationThatCannotExist(t *testing.T) {
	r, h, _ := newReplications()
	for _, tc := range []struct{ namespace, name string }{{"ns3", "nginx"}, {"ns1", "a.b"}, {"ns1", "a,b=c"}} {
		if _, err := getReplication(r, tc.namespace, tc.name); !apierrors.IsNotFound(err) {
			t.Errorf("%s/%s: got %v, want NotFound", tc.namespace, tc.name, err)
		}
	}
	if len(h.calls) != 0 {
		t.Errorf("calls %v, want none", h.calls)
	}
}

func TestGetInvisibleReplication(t *testing.T) {
	r, h, _ := newReplications()
	create(t, r, "ns1", replication("nginx"))
	p, _ := h.policy("k8s.proj.ns1.nginx")
	p.DestNamespace = "other/k8s/ns1/nginx"
	h.put(p)
	if _, err := getReplication(r, "ns1", "nginx"); !apierrors.IsNotFound(err) {
		t.Errorf("got %v, want NotFound", err)
	}
}

func TestGetReplicationHarborErrors(t *testing.T) {
	for _, method := range []string{"ListReplicationPolicies", "LatestReplicationExecution"} {
		t.Run(method, func(t *testing.T) {
			r, h, _ := newReplications()
			create(t, r, "ns1", replication("nginx"))
			h.errs = map[string]error{method: fmt.Errorf("%w: %w", harbor.ErrUnavailable, errLeak)}
			_, err := getReplication(r, "ns1", "nginx")
			checkHarborError(t, err, http.StatusServiceUnavailable, "harbor is unavailable")
		})
	}
}

func TestListReplications(t *testing.T) {
	r, h, n := newReplications()
	create(t, r, "ns2", replication("web"))
	create(t, r, "ns1", replication("web"))
	create(t, r, "ns1", replication("api"))
	n["ns3"] = namespace("ns3")
	create(t, r, "ns3", replication("web"))
	delete(n, "ns3")

	if diff := cmp.Diff([]string{"ns1/api", "ns1/web", "ns2/web"}, listReplications(t, r, "", nil)); diff != "" {
		t.Errorf("all (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"ns1/api", "ns1/web"}, listReplications(t, r, "ns1", nil)); diff != "" {
		t.Errorf("ns1 (-want +got):\n%s", diff)
	}
	h.calls = nil
	if got := listReplications(t, r, "ns3", nil); len(got) != 0 {
		t.Errorf("ns3: %v", got)
	}
	if len(h.calls) != 0 {
		t.Errorf("listing ns3 made calls %v", h.calls)
	}
}

func TestListReplicationsReadsExecutions(t *testing.T) {
	r, h, _ := newReplications()
	create(t, r, "ns1", replication("web"))
	h.finish("Succeed")
	h.calls = nil
	obj, err := r.List(inNamespace(""), nil)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"ListReplicationPolicies k8s.proj.", "LatestReplicationExecution 1"}, h.calls); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
	got := obj.(*v1alpha1.HarborReplicationList).Items[0]
	want, _ := getReplication(r, "ns1", "web")
	if diff := cmp.Diff(*want, got); diff != "" {
		t.Errorf("(-get +list):\n%s", diff)
	}
}

func TestListNoReplications(t *testing.T) {
	r, _, _ := newReplications()
	obj, err := r.List(inNamespace("ns1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if items := obj.(*v1alpha1.HarborReplicationList).Items; items == nil || len(items) != 0 {
		t.Errorf("items %#v, want empty", items)
	}
}

func TestListReplicationsWithSelectors(t *testing.T) {
	r, h, _ := newReplications()
	web := replication("web")
	web.Labels = map[string]string{"app": "web"}
	create(t, r, "ns1", web)
	create(t, r, "ns1", replication("api"))
	create(t, r, "ns2", replication("web"))
	for _, tc := range []struct {
		opts *metainternalversion.ListOptions
		want []string
	}{
		{&metainternalversion.ListOptions{LabelSelector: labels.SelectorFromSet(labels.Set{"app": "web"})}, []string{"ns1/web"}},
		{&metainternalversion.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", "web")}, []string{"ns1/web", "ns2/web"}},
		{&metainternalversion.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.namespace", "ns2")}, []string{"ns2/web"}},
	} {
		h.calls = nil
		if diff := cmp.Diff(tc.want, listReplications(t, r, "", tc.opts)); diff != "" {
			t.Errorf("%v (-want +got):\n%s", tc.opts, diff)
		}
		if len(h.calls) != 1+len(tc.want) {
			t.Errorf("%v: calls %v, want executions only of the selected replications", tc.opts, h.calls)
		}
	}
}

func TestListReplicationsReadsFourExecutionsAtOnce(t *testing.T) {
	r, h, _ := newReplications()
	for i := range 10 {
		create(t, r, "ns1", replication(fmt.Sprintf("r%d", i)))
	}
	h.delay = 10 * time.Millisecond
	if got := listReplications(t, r, "ns1", nil); len(got) != 10 {
		t.Fatalf("listed %v", got)
	}
	if h.maxInFlight != 4 {
		t.Errorf("read %d executions at once, want 4", h.maxInFlight)
	}
}

func TestListReplicationsHarborErrors(t *testing.T) {
	for _, method := range []string{"ListReplicationPolicies", "LatestReplicationExecution"} {
		t.Run(method, func(t *testing.T) {
			r, h, _ := newReplications()
			create(t, r, "ns1", replication("nginx"))
			h.errs = map[string]error{method: fmt.Errorf("%w: %w", harbor.ErrForbidden, errLeak)}
			_, err := r.List(inNamespace(""), nil)
			checkHarborError(t, err, http.StatusInternalServerError, "harbor denied the robot account access")
		})
	}
}

func deleteReplication(r *Replications, namespace, name string, options *metav1.DeleteOptions) (*v1alpha1.HarborReplication, error) {
	obj, immediate, err := r.Delete(inNamespace(namespace), name, nil, options)
	if err != nil {
		return nil, err
	}
	if !immediate {
		return nil, errors.New("not deleted immediately")
	}
	return obj.(*v1alpha1.HarborReplication), nil
}

func TestDeleteReplication(t *testing.T) {
	r, h, _ := newReplications()
	create(t, r, "ns1", replication("nginx"))
	h.finish("Succeed")
	want, _ := getReplication(r, "ns1", "nginx")
	h.calls = nil
	got, err := deleteReplication(r, "ns1", "nginx", &metav1.DeleteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"DeleteReplicationPolicy 1"}, h.writes()); diff != "" {
		t.Errorf("writes (-want +got):\n%s", diff)
	}
	if _, err := getReplication(r, "ns1", "nginx"); !apierrors.IsNotFound(err) {
		t.Errorf("get after delete: got %v, want NotFound", err)
	}
}

func TestDeleteRunningReplication(t *testing.T) {
	r, h, _ := newReplications()
	create(t, r, "ns1", replication("nginx"))
	h.calls = nil
	if _, err := deleteReplication(r, "ns1", "nginx", nil); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"DeleteReplicationPolicy 1", "StopReplicationExecution 2", "DeleteReplicationPolicy 1"}, h.writes()); diff != "" {
		t.Errorf("writes (-want +got):\n%s", diff)
	}
}

func TestDeleteReplicationWhileAnOlderExecutionRuns(t *testing.T) {
	r, h, _ := newReplications()
	create(t, r, "ns1", replication("nginx"))
	if _, err := h.StartReplication(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if got, _ := getReplication(r, "ns1", "nginx"); got.Status.LastExecution.Phase != v1alpha1.ReplicationPhaseFailed {
		t.Fatalf("latest execution %+v, want the skipped one", got.Status.LastExecution)
	}
	h.calls = nil
	if _, err := deleteReplication(r, "ns1", "nginx", nil); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"DeleteReplicationPolicy 1", "StopReplicationExecution 2", "DeleteReplicationPolicy 1"}, h.writes()); diff != "" {
		t.Errorf("writes (-want +got):\n%s", diff)
	}
}

func TestDeleteReplicationWhileItStops(t *testing.T) {
	r, h, _ := newReplications()
	create(t, r, "ns1", replication("nginx"))
	h.busyDeletes = 2
	if _, err := deleteReplication(r, "ns1", "nginx", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.policy("k8s.proj.ns1.nginx"); ok {
		t.Error("the policy remains")
	}
}

func TestDeleteReplicationThatDoesNotStop(t *testing.T) {
	r, h, _ := newReplications()
	create(t, r, "ns1", replication("nginx"))
	h.ignoreStops = true
	h.calls = nil
	start := time.Now()
	_, err := deleteReplication(r, "ns1", "nginx", nil)
	if !apierrors.IsConflict(err) || !strings.Contains(err.Error(), "retry") {
		t.Errorf("got %v, want a Conflict that asks to retry", err)
	}
	if took := time.Since(start); took < r.stopWait {
		t.Errorf("gave up after %v, want %v", took, r.stopWait)
	}
	if stops := slices.DeleteFunc(h.writes(), func(w string) bool { return w != "StopReplicationExecution 2" }); len(stops) != 1 {
		t.Errorf("stopped the execution %d times, want once", len(stops))
	}
	if _, ok := h.policy("k8s.proj.ns1.nginx"); !ok {
		t.Error("the policy is gone")
	}

	r.stopWait = time.Hour
	ctx, cancel := context.WithTimeout(inNamespace("ns1"), 50*time.Millisecond)
	defer cancel()
	if _, _, err := r.Delete(ctx, "nginx", nil, nil); !apierrors.IsConflict(err) {
		t.Errorf("when the request ends: got %v, want Conflict", err)
	}
}

func TestDeleteReplicationPreconditions(t *testing.T) {
	r, h, _ := newReplications()
	created := create(t, r, "ns1", replication("nginx"))
	h.finish("Succeed")
	current, _ := getReplication(r, "ns1", "nginx")
	for _, p := range []*metav1.Preconditions{
		{UID: ptr.To[types.UID]("other")},
		{ResourceVersion: ptr.To(created.ResourceVersion)},
	} {
		if _, err := deleteReplication(r, "ns1", "nginx", &metav1.DeleteOptions{Preconditions: p}); !apierrors.IsConflict(err) {
			t.Errorf("%+v: got %v, want Conflict", p, err)
		}
	}
	if w := h.writes(); len(w) != 2 {
		t.Errorf("writes %v, want none after create", w)
	}
	p := &metav1.Preconditions{UID: ptr.To(current.UID), ResourceVersion: ptr.To(current.ResourceVersion)}
	if _, err := deleteReplication(r, "ns1", "nginx", &metav1.DeleteOptions{Preconditions: p}); err != nil {
		t.Error(err)
	}
}

func TestDeleteReplicationThatAdmissionRejects(t *testing.T) {
	r, h, _ := newReplications()
	create(t, r, "ns1", replication("nginx"))
	h.calls = nil
	rejected := apierrors.NewForbidden(replicationsResource, "nginx", errors.New("no"))
	var validated runtime.Object
	_, _, err := r.Delete(inNamespace("ns1"), "nginx", func(_ context.Context, o runtime.Object) error {
		validated = o
		return rejected
	}, nil)
	if err != rejected {
		t.Errorf("got %v, want %v", err, rejected)
	}
	if validated.(*v1alpha1.HarborReplication).Name != "nginx" {
		t.Errorf("validated %+v", validated)
	}
	if w := h.writes(); len(w) != 0 {
		t.Errorf("writes %v, want none", w)
	}
}

func TestDeleteReplicationDryRun(t *testing.T) {
	r, h, _ := newReplications()
	create(t, r, "ns1", replication("nginx"))
	h.calls = nil
	got, err := deleteReplication(r, "ns1", "nginx", &metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "nginx" {
		t.Errorf("got %+v", got)
	}
	if w := h.writes(); len(w) != 0 {
		t.Errorf("writes %v, want none", w)
	}
}

func TestDeleteInvisibleReplication(t *testing.T) {
	r, h, _ := newReplications()
	create(t, r, "ns1", replication("nginx"))
	p, _ := h.policy("k8s.proj.ns1.nginx")
	withDescription(&p, func(d *policyDescription) { d.Spec.Registry = "other" })
	p.SrcRegistry = &harbor.Registry{ID: 9, Name: "other"}
	h.put(p)
	h.calls = nil
	for _, tc := range []struct{ namespace, name string }{{"ns1", "nginx"}, {"ns2", "nginx"}, {"ns3", "nginx"}} {
		if _, err := deleteReplication(r, tc.namespace, tc.name, nil); !apierrors.IsNotFound(err) {
			t.Errorf("%s/%s: got %v, want NotFound", tc.namespace, tc.name, err)
		}
	}
	if w := h.writes(); len(w) != 0 {
		t.Errorf("writes %v, want none", w)
	}
}

func TestDeleteReplicationHarborErrors(t *testing.T) {
	for _, tc := range []struct {
		method string
		err    error
		check  func(error) bool
	}{
		{"ListRunningReplicationExecutions", harbor.ErrUnavailable, apierrors.IsServiceUnavailable},
		{"StopReplicationExecution", harbor.ErrUnavailable, apierrors.IsServiceUnavailable},
		{"StopReplicationExecution", harbor.ErrNotFound, apierrors.IsNotFound},
		{"DeleteReplicationPolicy", harbor.ErrUnavailable, apierrors.IsServiceUnavailable},
		{"DeleteReplicationPolicy", harbor.ErrNotFound, apierrors.IsNotFound},
	} {
		t.Run(fmt.Sprintf("%s %v", tc.method, tc.err), func(t *testing.T) {
			r, h, _ := newReplications()
			create(t, r, "ns1", replication("nginx"))
			h.errs = map[string]error{tc.method: tc.err}
			if _, err := deleteReplication(r, "ns1", "nginx", nil); !tc.check(err) {
				t.Errorf("got %v", err)
			}
		})
	}
}

func TestReplicationTable(t *testing.T) {
	r, h, _ := newReplications()
	scheduled := replication("web")
	scheduled.Spec.Schedule = "0 0 3 * * *"
	create(t, r, "ns1", scheduled)
	h.errs = map[string]error{"StartReplication": harbor.ErrUnavailable}
	create(t, r, "ns1", replication("api"))
	list, err := r.List(inNamespace("ns1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	table, err := r.ConvertToTable(context.Background(), list, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range table.ColumnDefinitions {
		names = append(names, c.Name)
	}
	if diff := cmp.Diff([]string{"Name", "Registry", "Repository", "Tag", "Schedule", "Phase", "Age"}, names); diff != "" {
		t.Errorf("columns (-want +got):\n%s", diff)
	}
	var cells [][]any
	for _, row := range table.Rows {
		cells = append(cells, row.Cells[:6])
	}
	want := [][]any{
		{"api", "hub", "library/api", "1.27*", "<none>", "<none>"},
		{"web", "hub", "library/web", "1.27*", "0 0 3 * * *", "InProgress"},
	}
	if diff := cmp.Diff(want, cells); diff != "" {
		t.Errorf("cells (-want +got):\n%s", diff)
	}
	if one, err := r.ConvertToTable(context.Background(), &list.(*v1alpha1.HarborReplicationList).Items[0], nil); err != nil || len(one.Rows) != 1 {
		t.Errorf("one replication: %v, %v", one, err)
	}
}

func TestReplicationsResource(t *testing.T) {
	r, _, _ := newReplications()
	if _, ok := r.New().(*v1alpha1.HarborReplication); !ok {
		t.Errorf("New returns %T", r.New())
	}
	if _, ok := r.NewList().(*v1alpha1.HarborReplicationList); !ok {
		t.Errorf("NewList returns %T", r.NewList())
	}
	if !r.NamespaceScoped() || r.GetSingularName() != "harborreplication" {
		t.Error("not the namespaced resource harborreplication")
	}
}
