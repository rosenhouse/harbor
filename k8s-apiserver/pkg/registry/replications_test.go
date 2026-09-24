package registry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bmatcuk/doublestar"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/utils/ptr"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

func newReplications() (*Replications, *fakeReplicationHarbor, namespaceObjects) {
	h := &fakeReplicationHarbor{registries: []harbor.Registry{
		{ID: 3, Name: "hub"},
		{ID: 5, Name: "quay"},
		{ID: 9, Name: "other"},
	}}
	n := namespacesNamed("ns1", "ns2")
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

func TestCreateReplicationOverPolicyInAnotherProject(t *testing.T) {
	r, h, _ := newReplications()
	h.robotProject = "proj"
	h.put(harbor.ReplicationPolicy{ID: 99, Name: "k8s.proj.ns1.nginx", DestNamespace: "other/k8s/ns1/nginx"})

	_, err := r.Create(inNamespace("ns1"), replication("nginx"), nil, &metav1.CreateOptions{})

	if !apierrors.IsAlreadyExists(err) || !strings.Contains(err.Error(), "policy k8s.proj.ns1.nginx") || !strings.Contains(err.Error(), "a Harbor administrator must delete it") {
		t.Errorf("got %v, want AlreadyExists that names the unreadable policy", err)
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
		"no tag":                               {change: func(o *v1alpha1.HarborReplication) { o.Spec.Tag = "" }, want: []string{"spec.tag"}},
		"long tag":                             {change: func(o *v1alpha1.HarborReplication) { o.Spec.Tag = strings.Repeat("a", 129) }, want: []string{"spec.tag"}},
		"tag pattern":                          {change: func(o *v1alpha1.HarborReplication) { o.Spec.Tag = "V_[0-9][^a-z]?.*-*" }},
		"tag pattern with three stars":         {change: func(o *v1alpha1.HarborReplication) { o.Spec.Tag = "*?*?*Z" }, want: []string{"spec.tag"}},
		"tag pattern with many stars":          {change: func(o *v1alpha1.HarborReplication) { o.Spec.Tag = strings.Repeat("*?", 12) + "Z" }, want: []string{"spec.tag"}},
		"alternatives":                         {change: func(o *v1alpha1.HarborReplication) { o.Spec.Tag = "v{1,2}" }, want: []string{"spec.tag"}},
		"unclosed character class":             {change: func(o *v1alpha1.HarborReplication) { o.Spec.Tag = "v[" }, want: []string{"spec.tag"}},
		"empty character class":                {change: func(o *v1alpha1.HarborReplication) { o.Spec.Tag = "v[]" }, want: []string{"spec.tag"}},
		"open character range":                 {change: func(o *v1alpha1.HarborReplication) { o.Spec.Tag = "v[0-]" }, want: []string{"spec.tag"}},
		"tag that JSON escapes":                {change: func(o *v1alpha1.HarborReplication) { o.Spec.Tag = "<" }, want: []string{"spec.tag"}},
		"tag with a space":                     {change: func(o *v1alpha1.HarborReplication) { o.Spec.Tag = "1 2" }, want: []string{"spec.tag"}},
		"tag with a slash":                     {change: func(o *v1alpha1.HarborReplication) { o.Spec.Tag = "a/b" }, want: []string{"spec.tag"}},
		"tag with an escape":                   {change: func(o *v1alpha1.HarborReplication) { o.Spec.Tag = `\*` }, want: []string{"spec.tag"}},
		"five schedule fields":                 {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 3 * * *" }, want: []string{"spec.schedule"}},
		"seven schedule fields":                {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 0 3 * * * 2026" }, want: []string{"spec.schedule"}},
		"schedule time zone":                   {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "TZ=UTC 0 0 3 * * *" }, want: []string{"spec.schedule"}},
		"schedule descriptor":                  {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "@hourly" }, want: []string{"spec.schedule"}},
		"double space in schedule":             {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0  0 3 * * *" }, want: []string{"spec.schedule"}},
		"trailing space in schedule":           {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 0 3 * * * " }, want: []string{"spec.schedule"}},
		"schedule out of range":                {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 0 24 * * *" }, want: []string{"spec.schedule"}},
		"schedule minute out of range":         {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 60 3 * * *" }, want: []string{"spec.schedule"}},
		"schedule seconds":                     {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "30 0 3 * * *" }, want: []string{"spec.schedule"}},
		"schedule every minute":                {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 * 3 * * *" }, want: []string{"spec.schedule"}},
		"schedule minute step":                 {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 */5 3 * * *" }, want: []string{"spec.schedule"}},
		"schedule minute list":                 {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 0,30 3 * * *" }, want: []string{"spec.schedule"}},
		"schedule minute range":                {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 1-2 3 * * *" }, want: []string{"spec.schedule"}},
		"schedule minute with a sign":          {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 +5 3 * * *" }, want: []string{"spec.schedule"}},
		"schedule on February 29":              {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 0 0 29 2 *" }},
		"schedule on February 30":              {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 0 0 30 2 *" }, want: []string{"spec.schedule"}},
		"schedule on April 31":                 {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 0 0 31 4 *" }, want: []string{"spec.schedule"}},
		"schedule on a weekday of February 30": {change: func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 0 0 30 2 1" }},
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
		"metadata that fits in size": {change: func(o *v1alpha1.HarborReplication) {
			o.Labels = map[string]string{"a": strings.Repeat("b", 63)}
			o.Annotations = map[string]string{"c": strings.Repeat("<", 8192-64-1)}
		}},
		"large metadata": {change: func(o *v1alpha1.HarborReplication) {
			o.Labels = map[string]string{"a": strings.Repeat("b", 63)}
			o.Annotations = map[string]string{"c": strings.Repeat("<", 8192-64)}
		}, want: []string{"metadata"}},
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

// Harbor matches tags with doublestar v1.3.4, which src/go.mod pins.
func TestHarborMatchesValidTagPatternsQuickly(t *testing.T) {
	const maxMatchTime = 50 * time.Millisecond
	tag := strings.Repeat("a", maxTagLength)
	for _, pattern := range []string{
		"**" + strings.Repeat("?", 125) + "b",
		"*a*" + strings.Repeat("?", 124) + "b",
		"**" + strings.Repeat("[a]", 42) + "b",
		"**" + strings.Repeat("[^b]", 31) + "b",
		"**[" + strings.Repeat("a", 123) + "]b",
		"***" + strings.Repeat("[a]", 41) + "b",
		"**{" + strings.Repeat(",", 80) + "}" + strings.Repeat("?", 40) + "b",
		"*{*,*}" + strings.Repeat("?", 120) + "b",
	} {
		obj := replication("nginx")
		obj.Namespace, obj.Spec.Tag = "ns1", pattern
		if len(replicationConfig.validate(obj)) > 0 {
			continue
		}
		fastest := time.Duration(math.MaxInt64)
		for range 3 {
			start := time.Now()
			if _, err := doublestar.Match(pattern, tag); err != nil {
				t.Fatalf("pattern %q: %v", pattern, err)
			}
			fastest = min(fastest, time.Since(start))
		}
		if fastest > maxMatchTime {
			t.Errorf("pattern %q took %v to match a tag, want at most %v", pattern, fastest, maxMatchTime)
		}
	}
}

func TestCreateReplicationInNamespaceWithoutProject(t *testing.T) {
	r, h, _ := newReplications()
	_, err := r.Create(inNamespace("ns3"), replication("nginx"), nil, &metav1.CreateOptions{})
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "namespace ns3 does not exist, or is not labeled") {
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

func TestCreateReplicationWhoseExecutionCannotBeRead(t *testing.T) {
	r, h, _ := newReplications()
	h.errs = map[string]error{"LatestReplicationExecution": harbor.ErrUnavailable}
	out := create(t, r, "ns1", replication("nginx"))
	if out.Status.LastExecution != nil {
		t.Errorf("last execution %+v, want none", out.Status.LastExecution)
	}
	if diff := cmp.Diff([]string{"CreateReplicationPolicy k8s.proj.ns1.nginx", "StartReplication 1"}, h.writes()); diff != "" {
		t.Errorf("writes (-want +got):\n%s", diff)
	}
}

// A create that fails after Harbor may have stored the policy deletes the policy, so that a retry creates and starts it.
func TestCreateReplicationThatFailsInHarbor(t *testing.T) {
	for name, tc := range map[string]struct {
		errs      map[string]error
		lostReply bool
		writes    []string
	}{
		"policy not stored":           {map[string]error{"CreateReplicationPolicy": harbor.ErrUnavailable}, false, []string{"CreateReplicationPolicy k8s.proj.ns1.nginx"}},
		"reply to the create lost":    {nil, true, []string{"CreateReplicationPolicy k8s.proj.ns1.nginx", "DeleteReplicationPolicy 1"}},
		"reply lost, policy unlisted": {map[string]error{"ListReplicationPolicies": harbor.ErrUnavailable}, true, []string{"CreateReplicationPolicy k8s.proj.ns1.nginx"}},
		"policy unreadable":           {map[string]error{"GetReplicationPolicy": harbor.ErrUnavailable}, false, []string{"CreateReplicationPolicy k8s.proj.ns1.nginx", "DeleteReplicationPolicy 1"}},
		"run not started":             {map[string]error{"StartReplication": harbor.ErrUnavailable}, false, []string{"CreateReplicationPolicy k8s.proj.ns1.nginx", "StartReplication 1", "DeleteReplicationPolicy 1"}},
		"run not started, undeleted":  {map[string]error{"StartReplication": harbor.ErrUnavailable, "DeleteReplicationPolicy": harbor.ErrUnavailable}, false, []string{"CreateReplicationPolicy k8s.proj.ns1.nginx", "StartReplication 1", "DeleteReplicationPolicy 1"}},
	} {
		t.Run(name, func(t *testing.T) {
			r, h, _ := newReplications()
			h.errs, h.lostReply = tc.errs, tc.lostReply
			_, err := r.Create(inNamespace("ns1"), replication("nginx"), nil, &metav1.CreateOptions{})
			checkHarborError(t, err, http.StatusServiceUnavailable, "harbor is unavailable")
			if diff := cmp.Diff(tc.writes, h.writes()); diff != "" {
				t.Errorf("writes (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCreateReplicationWhoseRequestEnds(t *testing.T) {
	r, h, _ := newReplications()
	ctx, cancel := context.WithCancel(inNamespace("ns1"))
	h.failEndedRequests, h.onCreate = true, cancel
	_, err := r.Create(ctx, replication("nginx"), nil, &metav1.CreateOptions{})
	checkHarborError(t, err, http.StatusServiceUnavailable, "harbor is unavailable")
	if _, ok := h.policy("k8s.proj.ns1.nginx"); ok {
		t.Error("the policy is left")
	}
}

func TestCreateReplicationThatFailsKeepsAnotherRequestsPolicy(t *testing.T) {
	r, h, _ := newReplications()
	create(t, r, "ns1", replication("nginx"))
	h.calls = nil
	h.errs = map[string]error{"CreateReplicationPolicy": harbor.ErrUnavailable}
	_, err := r.Create(inNamespace("ns1"), replication("nginx"), nil, &metav1.CreateOptions{})
	checkHarborError(t, err, http.StatusServiceUnavailable, "harbor is unavailable")
	if diff := cmp.Diff([]string{"CreateReplicationPolicy k8s.proj.ns1.nginx"}, h.writes()); diff != "" {
		t.Errorf("writes (-want +got):\n%s", diff)
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

// Requests in one namespace never reach another's replications, even from a Harbor client that ignores the name prefix.
func TestReplicationsStayInTheirNamespace(t *testing.T) {
	r, h, _ := newReplications()
	create(t, r, "ns2", replication("nginx"))
	h.ignorePrefix = true
	if _, err := getReplication(r, "ns1", "nginx"); !apierrors.IsNotFound(err) {
		t.Errorf("get: got %v, want NotFound", err)
	}
	if got := listReplications(t, r, "ns1", nil); len(got) != 0 {
		t.Errorf("list: got %v", got)
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

func TestListReplicationsInNamespaceListsOnlyItsPolicies(t *testing.T) {
	r, h, _ := newReplications()
	create(t, r, "ns1", replication("web"))
	h.calls = nil
	listReplications(t, r, "ns1", nil)
	if diff := cmp.Diff([]string{"ListReplicationPolicies k8s.proj.ns1.", "LatestReplicationExecution 1"}, h.calls); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
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
	h.calls = nil
	if _, err := deleteReplication(r, "ns1", "nginx", nil); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"DeleteReplicationPolicy 1", "StopReplicationExecution 2", "DeleteReplicationPolicy 1"}, h.writes()); diff != "" {
		t.Errorf("writes (-want +got):\n%s", diff)
	}
}

func TestDeleteReplicationWhoseExecutionEndsBeforeTheStop(t *testing.T) {
	r, h, _ := newReplications()
	create(t, r, "ns1", replication("nginx"))
	h.errs = map[string]error{"StopReplicationExecution": harbor.ErrNotFound}
	if _, err := deleteReplication(r, "ns1", "nginx", nil); !apierrors.IsConflict(err) {
		t.Errorf("got %v, want the Conflict of a policy whose executions have not stopped", err)
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
	h.calls = nil
	for _, p := range []*metav1.Preconditions{
		{UID: ptr.To[types.UID]("other")},
		{ResourceVersion: ptr.To(created.ResourceVersion)},
	} {
		if _, err := deleteReplication(r, "ns1", "nginx", &metav1.DeleteOptions{Preconditions: p}); !apierrors.IsConflict(err) {
			t.Errorf("%+v: got %v, want Conflict", p, err)
		}
	}
	if w := h.writes(); len(w) != 0 {
		t.Errorf("writes %v, want none", w)
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

// patch returns what the API server passes to Update for a patch that changes the current object.
// Like the API server's, it fails with NotFound on an object without a UID.
func patch(change func(*v1alpha1.HarborReplication)) rest.UpdatedObjectInfo {
	return rest.DefaultUpdatedObjectInfo(nil, func(_ context.Context, _, current runtime.Object) (runtime.Object, error) {
		obj := current.(*v1alpha1.HarborReplication).DeepCopy()
		if obj.UID == "" {
			return nil, apierrors.NewNotFound(replicationsResource, obj.Name)
		}
		change(obj)
		return obj, nil
	})
}

func updateReplication(r *Replications, namespace, name string, objInfo rest.UpdatedObjectInfo, forceAllowCreate bool, options *metav1.UpdateOptions) (*v1alpha1.HarborReplication, error) {
	obj, created, err := r.Update(inNamespace(namespace), name, objInfo, nil, nil, forceAllowCreate, options)
	if err != nil {
		return nil, err
	}
	if created {
		return nil, errors.New("created")
	}
	return obj.(*v1alpha1.HarborReplication), nil
}

// labeledReplication creates ns1/nginx with labels and annotations, and ends its execution.
func labeledReplication(t *testing.T) (*Replications, *fakeReplicationHarbor, *v1alpha1.HarborReplication) {
	t.Helper()
	r, h, _ := newReplications()
	obj := replication("nginx")
	obj.Labels = map[string]string{"app": "web"}
	obj.Annotations = map[string]string{"note": "x"}
	create(t, r, "ns1", obj)
	h.finish("Succeed")
	current, err := getReplication(r, "ns1", "nginx")
	if err != nil {
		t.Fatal(err)
	}
	h.calls = nil
	return r, h, current
}

func TestUpdateReplicationWithoutChanges(t *testing.T) {
	for name, change := range map[string]func(*v1alpha1.HarborReplication){
		"nothing": func(*v1alpha1.HarborReplication) {},
		"managed fields": func(o *v1alpha1.HarborReplication) {
			o.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply}}
		},
		"status": func(o *v1alpha1.HarborReplication) {
			o.Status = v1alpha1.HarborReplicationStatus{Destination: "elsewhere"}
		},
		"no resource version": func(o *v1alpha1.HarborReplication) { o.ResourceVersion = "" },
		"empty finalizers":    func(o *v1alpha1.HarborReplication) { o.Finalizers = []string{} },
		"manifest": func(o *v1alpha1.HarborReplication) {
			*o = v1alpha1.HarborReplication{ObjectMeta: metav1.ObjectMeta{Name: o.Name, Labels: o.Labels, Annotations: o.Annotations}, Spec: o.Spec}
		},
		// Client-side kubectl apply adds it to a replication that it did not create.
		"kubectl's last-applied configuration": func(o *v1alpha1.HarborReplication) {
			o.Annotations[corev1.LastAppliedConfigAnnotation] = "{}"
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, h, current := labeledReplication(t)
			got, err := updateReplication(r, "ns1", "nginx", patch(change), false, &metav1.UpdateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(current, got); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
			if w := h.writes(); len(w) != 0 {
				t.Errorf("writes %v, want none", w)
			}
		})
	}
}

func TestUpdateChangedReplication(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*v1alpha1.HarborReplication)
		fields []string
	}{
		"tag":         {func(o *v1alpha1.HarborReplication) { o.Spec.Tag = "1.28*" }, []string{"spec"}},
		"schedule":    {func(o *v1alpha1.HarborReplication) { o.Spec.Schedule = "0 0 3 * * *" }, []string{"spec"}},
		"new label":   {func(o *v1alpha1.HarborReplication) { o.Labels["team"] = "a" }, []string{"metadata.labels"}},
		"no labels":   {func(o *v1alpha1.HarborReplication) { o.Labels = nil }, []string{"metadata.labels"}},
		"annotations": {func(o *v1alpha1.HarborReplication) { o.Annotations["note"] = "y" }, []string{"metadata.annotations"}},
		"finalizers":  {func(o *v1alpha1.HarborReplication) { o.Finalizers = []string{"example.com/x"} }, []string{"metadata.finalizers"}},
		"owner references": {func(o *v1alpha1.HarborReplication) {
			o.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "x", UID: "u"}}
		}, []string{"metadata.ownerReferences"}},
		"uid":            {func(o *v1alpha1.HarborReplication) { o.UID = "other" }, []string{"metadata.uid"}},
		"spec and label": {func(o *v1alpha1.HarborReplication) { o.Spec.Tag, o.Labels = "*", nil }, []string{"metadata.labels", "spec"}},
	} {
		for _, options := range []*metav1.UpdateOptions{{}, {DryRun: []string{metav1.DryRunAll}}} {
			t.Run(fmt.Sprintf("%s %v", name, options.DryRun), func(t *testing.T) {
				r, h, _ := labeledReplication(t)
				_, err := updateReplication(r, "ns1", "nginx", patch(tc.change), false, options)
				var status apierrors.APIStatus
				if !apierrors.IsInvalid(err) || !errors.As(err, &status) {
					t.Fatalf("got %v, want Invalid", err)
				}
				var fields []string
				for _, c := range status.Status().Details.Causes {
					fields = append(fields, c.Field)
					if !strings.Contains(c.Message, ": field is immutable") {
						t.Errorf("%s: message %q, want the standard immutable message", c.Field, c.Message)
					}
				}
				if diff := cmp.Diff(tc.fields, fields); diff != "" {
					t.Errorf("fields (-want +got):\n%s", diff)
				}
				if w := h.writes(); len(w) != 0 {
					t.Errorf("writes %v, want none", w)
				}
			})
		}
	}
}

func TestUpdateChangedReplicationSaysHowToChangeIt(t *testing.T) {
	r, _, _ := labeledReplication(t)
	_, err := updateReplication(r, "ns1", "nginx", patch(func(o *v1alpha1.HarborReplication) {
		o.Spec.Tag = "1.28*"
		o.Annotations["note"] = strings.Repeat("y", 100)
	}), false, &metav1.UpdateOptions{})
	want := `HarborReplication.harbor.goharbor.io "nginx" is invalid: [` +
		`metadata.annotations: Invalid value: field is immutable; delete and recreate the replication to change it, ` +
		`spec: Invalid value: {"registry":"hub","repository":"library/nginx","tag":"1.28*"}: field is immutable; delete and recreate the replication to change it]`
	if err == nil || err.Error() != want {
		t.Errorf("got %v\nwant %s", err, want)
	}
}

func TestUpdateReplicationWithResourceVersion(t *testing.T) {
	r, h, current := labeledReplication(t)
	if _, err := updateReplication(r, "ns1", "nginx", patch(func(*v1alpha1.HarborReplication) {}), false, &metav1.UpdateOptions{}); err != nil {
		t.Errorf("current resource version: %v", err)
	}
	h.finish("Failed")
	_, err := updateReplication(r, "ns1", "nginx", patch(func(o *v1alpha1.HarborReplication) { o.ResourceVersion = current.ResourceVersion }), false, &metav1.UpdateOptions{})
	if !apierrors.IsConflict(err) || !strings.Contains(err.Error(), "the object has been modified") {
		t.Errorf("stale resource version: got %v, want Conflict", err)
	}
}

// An update's body sets a UID precondition.
func TestUpdateReplicationWithUIDPrecondition(t *testing.T) {
	r, h, current := labeledReplication(t)
	obj := current.DeepCopy()
	obj.UID = "other"
	_, err := updateReplication(r, "ns1", "nginx", rest.DefaultUpdatedObjectInfo(obj), false, &metav1.UpdateOptions{})
	if !apierrors.IsConflict(err) || !strings.Contains(err.Error(), "the UID in the precondition (other) does not match the UID in record ("+string(current.UID)+")") {
		t.Errorf("got %v, want Conflict", err)
	}
	if w := h.writes(); len(w) != 0 {
		t.Errorf("writes %v, want none", w)
	}
}

// Admission sees the current replication, even when the request leaves out or changes what the server fills in or ignores.
func TestUpdateReplicationThatAdmissionRejects(t *testing.T) {
	r, _, current := labeledReplication(t)
	rejected := apierrors.NewForbidden(replicationsResource, "nginx", errors.New("no"))
	var validated, validatedOld runtime.Object
	manifest := patch(func(o *v1alpha1.HarborReplication) {
		o.UID, o.ResourceVersion, o.CreationTimestamp, o.Generation = "", "", metav1.Time{}, 0
		o.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply}}
		o.Status = v1alpha1.HarborReplicationStatus{}
	})
	_, _, err := r.Update(inNamespace("ns1"), "nginx", manifest, nil, func(_ context.Context, o, old runtime.Object) error {
		validated, validatedOld = o, old
		return rejected
	}, false, &metav1.UpdateOptions{})
	if err != rejected {
		t.Errorf("got %v, want %v", err, rejected)
	}
	if diff := cmp.Diff(current, validated); diff != "" {
		t.Errorf("validated object (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(current, validatedOld); diff != "" {
		t.Errorf("validated old object (-want +got):\n%s", diff)
	}
}

func TestUpdateMissingReplication(t *testing.T) {
	r, h, _ := newReplications()
	for _, tc := range []struct{ namespace, name string }{{"ns1", "nginx"}, {"ns3", "nginx"}, {"ns1", "a.b"}} {
		if _, err := updateReplication(r, tc.namespace, tc.name, rest.DefaultUpdatedObjectInfo(replication(tc.name)), false, &metav1.UpdateOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("%s/%s: got %v, want NotFound", tc.namespace, tc.name, err)
		}
	}
	if w := h.writes(); len(w) != 0 {
		t.Errorf("writes %v, want none", w)
	}
}

func TestUpdateCreatesMissingReplicationWhenForced(t *testing.T) {
	r, h, _ := newReplications()
	obj := replication("nginx")
	obj.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply}}
	var validated *v1alpha1.HarborReplication
	createValidation := func(_ context.Context, o runtime.Object) error {
		validated = o.(*v1alpha1.HarborReplication)
		return nil
	}
	// Like genericregistry.Store, Update gives an empty old object, from which apply builds the new one.
	var old runtime.Object
	apply := rest.DefaultUpdatedObjectInfo(obj, func(_ context.Context, obj, o runtime.Object) (runtime.Object, error) {
		old = o
		return obj, nil
	})
	out, created, err := r.Update(inNamespace("ns1"), "nginx", apply, createValidation, nil, true, &metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(runtime.Object(&v1alpha1.HarborReplication{}), old); diff != "" {
		t.Errorf("old object (-want +got):\n%s", diff)
	}
	if !created {
		t.Error("not created")
	}
	if diff := cmp.Diff([]string{"CreateReplicationPolicy k8s.proj.ns1.nginx", "StartReplication 1"}, h.writes()); diff != "" {
		t.Errorf("writes (-want +got):\n%s", diff)
	}
	if validated == nil || validated.UID == "" || validated.ManagedFields != nil {
		t.Errorf("validated %+v, want a prepared object", validated)
	}
	got, err := getReplication(r, "ns1", "nginx")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(got, out); diff != "" {
		t.Errorf("(-get +update):\n%s", diff)
	}
}

// Two server-side applies can race to create a replication. The one that loses applies to the replication that the other created.
func TestUpdateCreatesReplicationThatAnotherRequestCreates(t *testing.T) {
	for name, tc := range map[string]struct {
		other *v1alpha1.HarborReplication
		check func(error) bool
	}{
		"same":      {replication("nginx"), func(err error) bool { return err == nil }},
		"different": {replication("web"), apierrors.IsInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			r, _, _ := newReplications()
			tc.other.Name = "nginx"
			var once sync.Once
			apply := rest.DefaultUpdatedObjectInfo(nil, func(_ context.Context, _, old runtime.Object) (runtime.Object, error) {
				once.Do(func() { create(t, r, "ns1", tc.other) })
				obj := old.(*v1alpha1.HarborReplication).DeepCopy()
				obj.Name, obj.Spec = "nginx", replication("nginx").Spec
				return obj, nil
			})
			out, err := updateReplication(r, "ns1", "nginx", apply, true, &metav1.UpdateOptions{})
			if !tc.check(err) {
				t.Fatalf("got %v", err)
			}
			if err != nil {
				return
			}
			got, err := getReplication(r, "ns1", "nginx")
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(got, out); diff != "" {
				t.Errorf("(-get +update):\n%s", diff)
			}
		})
	}
}

func TestUpdateCreatesMissingReplicationOverHiddenPolicy(t *testing.T) {
	r, _, n := newReplications()
	create(t, r, "ns1", replication("nginx"))
	n["ns1"] = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns1", UID: "recreated"}}
	_, _, err := r.Update(inNamespace("ns1"), "nginx", rest.DefaultUpdatedObjectInfo(replication("nginx")), nil, nil, true, &metav1.UpdateOptions{})
	if !apierrors.IsAlreadyExists(err) || !strings.Contains(err.Error(), "A Harbor administrator must delete it") {
		t.Errorf("got %v, want AlreadyExists that names the hidden policy", err)
	}
}

func TestUpdateCreatesMissingReplicationAsCreateDoes(t *testing.T) {
	invalid := replication("nginx")
	invalid.Spec.Registry = "other"
	rejected := apierrors.NewForbidden(replicationsResource, "nginx", errors.New("no"))
	for name, tc := range map[string]struct {
		obj              runtime.Object
		namespace        string
		createValidation rest.ValidateObjectFunc
		check            func(error) bool
	}{
		"invalid":           {invalid, "ns1", nil, apierrors.IsInvalid},
		"other kind":        {&v1alpha1.HarborArtifact{}, "ns1", nil, apierrors.IsBadRequest},
		"unlabeled":         {replication("nginx"), "ns3", nil, apierrors.IsForbidden},
		"admission rejects": {replication("nginx"), "ns1", func(context.Context, runtime.Object) error { return rejected }, func(err error) bool { return err == rejected }},
	} {
		t.Run(name, func(t *testing.T) {
			r, h, _ := newReplications()
			_, _, err := r.Update(inNamespace(tc.namespace), "nginx", rest.DefaultUpdatedObjectInfo(tc.obj), tc.createValidation, nil, true, &metav1.UpdateOptions{})
			if !tc.check(err) {
				t.Errorf("got %v", err)
			}
			if w := h.writes(); len(w) != 0 {
				t.Errorf("writes %v, want none", w)
			}
		})
	}
}

func TestUpdateCreatesMissingReplicationDryRun(t *testing.T) {
	r, h, _ := newReplications()
	out, created, err := r.Update(inNamespace("ns1"), "nginx", rest.DefaultUpdatedObjectInfo(replication("nginx")), nil, nil, true, &metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		t.Fatal(err)
	}
	if !created || out.(*v1alpha1.HarborReplication).Status.Destination != "proj/k8s/ns1/nginx" {
		t.Errorf("created %v, %+v", created, out)
	}
	if w := h.writes(); len(w) != 0 {
		t.Errorf("writes %v, want none", w)
	}
}

func TestUpdateReplicationHarborErrors(t *testing.T) {
	r, h, _ := labeledReplication(t)
	h.errs = map[string]error{"ListReplicationPolicies": fmt.Errorf("%w: %w", harbor.ErrUnavailable, errLeak)}
	_, _, err := r.Update(inNamespace("ns1"), "nginx", rest.DefaultUpdatedObjectInfo(replication("nginx")), nil, nil, true, &metav1.UpdateOptions{})
	checkHarborError(t, err, http.StatusServiceUnavailable, "harbor is unavailable")
	if w := h.writes(); len(w) != 0 {
		t.Errorf("writes %v, want none", w)
	}
}

func TestUpdateReplicationToOtherKind(t *testing.T) {
	r, _, _ := labeledReplication(t)
	if _, err := updateReplication(r, "ns1", "nginx", rest.DefaultUpdatedObjectInfo(&v1alpha1.HarborArtifact{}), false, &metav1.UpdateOptions{}); !apierrors.IsBadRequest(err) {
		t.Errorf("got %v, want BadRequest", err)
	}
}

func TestReplicationTable(t *testing.T) {
	r, h, _ := newReplications()
	scheduled := replication("web")
	scheduled.Spec.Schedule = "0 0 3 * * *"
	create(t, r, "ns1", scheduled)
	create(t, r, "ns1", replication("api"))
	api, _ := h.policy("k8s.proj.ns1.api")
	h.executions = slices.DeleteFunc(h.executions, func(e harbor.ReplicationExecution) bool { return e.PolicyID == api.ID })
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
