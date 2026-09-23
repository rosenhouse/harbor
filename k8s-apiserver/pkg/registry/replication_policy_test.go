package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

var replicationConfig = ReplicationConfig{Project: "proj", Prefix: "k8s", Registries: []string{"hub", "quay", "missing"}}

func nginxReplication() *v1alpha1.HarborReplication {
	return &v1alpha1.HarborReplication{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "nginx",
			Namespace:   "ns1",
			UID:         "nginx-uid",
			Labels:      map[string]string{"app": "web"},
			Annotations: map[string]string{"note": "x"},
		},
		Spec: v1alpha1.HarborReplicationSpec{Registry: "hub", Repository: "library/nginx", Tag: "1.27*", Schedule: "0 0 3 * * *"},
	}
}

func TestReplicationPolicy(t *testing.T) {
	d := describe(nginxReplication(), "uid-ns1")
	want := &harbor.ReplicationPolicy{
		Name:                      "k8s.proj.ns1.nginx",
		Description:               `{"managedBy":"harbor-apiserver","namespace":"ns1","namespaceUID":"uid-ns1","name":"nginx","uid":"nginx-uid","labels":{"app":"web"},"annotations":{"note":"x"},"spec":{"registry":"hub","repository":"library/nginx","tag":"1.27*","schedule":"0 0 3 * * *"}}`,
		SrcRegistry:               &harbor.Registry{ID: 3},
		DestNamespace:             "proj/k8s/ns1/nginx",
		DestNamespaceReplaceCount: ptr.To[int8](0),
		Trigger:                   &harbor.ReplicationTrigger{Type: "scheduled", Settings: &harbor.ReplicationTriggerSettings{Cron: "0 0 3 * * *"}},
		Filters: []harbor.ReplicationFilter{
			{Type: "name", Value: "library/nginx"},
			{Type: "tag", Value: "1.27*", Decoration: "matches"},
		},
		Override:                true,
		Enabled:                 true,
		SingleActiveReplication: true,
	}
	if diff := cmp.Diff(want, replicationConfig.policy(d, 3)); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestUnscheduledReplicationPolicy(t *testing.T) {
	obj := nginxReplication()
	obj.Spec.Schedule = ""
	obj.Labels, obj.Annotations = nil, nil
	p := replicationConfig.policy(describe(obj, "uid-ns1"), 3)
	if diff := cmp.Diff(&harbor.ReplicationTrigger{Type: "manual"}, p.Trigger); diff != "" {
		t.Errorf("trigger (-want +got):\n%s", diff)
	}
	want := `{"managedBy":"harbor-apiserver","namespace":"ns1","namespaceUID":"uid-ns1","name":"nginx","uid":"nginx-uid","spec":{"registry":"hub","repository":"library/nginx","tag":"1.27*"}}`
	if p.Description != want {
		t.Errorf("description %s, want %s", p.Description, want)
	}
}

// storedPolicy returns nginxReplication's policy as Harbor returns it.
func storedPolicy() *harbor.ReplicationPolicy {
	p := replicationConfig.policy(describe(nginxReplication(), "uid-ns1"), 3)
	p.ID = 7
	p.SrcRegistry = &harbor.Registry{ID: 3, Name: "hub", Type: "docker-hub"}
	p.DestRegistry = &harbor.Registry{ID: 0, Name: "Local", Type: "harbor"}
	p.CreationTime = created
	return p
}

func withDescription(p *harbor.ReplicationPolicy, change func(*policyDescription)) {
	var d policyDescription
	if err := json.Unmarshal([]byte(p.Description), &d); err != nil {
		panic(err)
	}
	change(&d)
	b, err := json.Marshal(d)
	if err != nil {
		panic(err)
	}
	p.Description = string(b)
}

func TestVisiblePolicy(t *testing.T) {
	d, ok := replicationConfig.visible(storedPolicy(), namespaceObjects{"ns1": namespace("ns1")})
	if !ok {
		t.Fatal("not visible")
	}
	if diff := cmp.Diff(describe(nginxReplication(), "uid-ns1"), d); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}

	p := storedPolicy()
	p.DestRegistry = nil
	if _, ok := replicationConfig.visible(p, namespaceObjects{"ns1": namespace("ns1")}); !ok {
		t.Error("a policy without a destination registry is not visible")
	}
}

func TestInvisiblePolicies(t *testing.T) {
	for name, change := range map[string]func(p *harbor.ReplicationPolicy, n namespaceObjects){
		"another prefix": func(p *harbor.ReplicationPolicy, _ namespaceObjects) {
			p.Name = "other.proj.ns1.nginx"
		},
		"a description that is not JSON": func(p *harbor.ReplicationPolicy, _ namespaceObjects) {
			p.Description = "nginx"
		},
		"another manager": func(p *harbor.ReplicationPolicy, _ namespaceObjects) {
			withDescription(p, func(d *policyDescription) { d.ManagedBy = "someone" })
		},
		"another name": func(p *harbor.ReplicationPolicy, _ namespaceObjects) {
			p.Name = "k8s.proj.ns1.web"
		},
		"a described name that is not a DNS label": func(p *harbor.ReplicationPolicy, n namespaceObjects) {
			n["team"] = namespace("team")
			withDescription(p, func(d *policyDescription) { d.Namespace, d.NamespaceUID, d.Name = "team", "uid-team", "a.nginx" })
			p.Name = "k8s.proj.team.a.nginx"
			p.DestNamespace = "proj/k8s/team/a.nginx"
		},
		"another destination": func(p *harbor.ReplicationPolicy, _ namespaceObjects) {
			p.DestNamespace = "proj/k8s/ns1/nginx/x"
		},
		"no replace count": func(p *harbor.ReplicationPolicy, _ namespaceObjects) {
			p.DestNamespaceReplaceCount = nil
		},
		"a replace count": func(p *harbor.ReplicationPolicy, _ namespaceObjects) {
			p.DestNamespaceReplaceCount = ptr.To[int8](1)
		},
		"no source registry": func(p *harbor.ReplicationPolicy, _ namespaceObjects) {
			p.SrcRegistry = nil
		},
		"a local source": func(p *harbor.ReplicationPolicy, _ namespaceObjects) {
			p.SrcRegistry = &harbor.Registry{ID: 0, Name: "hub"}
		},
		"a source that is not allowed": func(p *harbor.ReplicationPolicy, _ namespaceObjects) {
			withDescription(p, func(d *policyDescription) { d.Spec.Registry = "other" })
			p.SrcRegistry = &harbor.Registry{ID: 9, Name: "other"}
		},
		"a source other than the described one": func(p *harbor.ReplicationPolicy, _ namespaceObjects) {
			p.SrcRegistry = &harbor.Registry{ID: 5, Name: "quay"}
		},
		"another repository filter": func(p *harbor.ReplicationPolicy, _ namespaceObjects) {
			p.Filters[0].Value = "**"
		},
		"another tag decoration": func(p *harbor.ReplicationPolicy, _ namespaceObjects) {
			p.Filters[1].Decoration = "excludes"
		},
		"another filter": func(p *harbor.ReplicationPolicy, _ namespaceObjects) {
			p.Filters = append(p.Filters, harbor.ReplicationFilter{Type: "label", Value: []any{"x"}})
		},
		"another schedule": func(p *harbor.ReplicationPolicy, _ namespaceObjects) {
			p.Trigger.Settings.Cron = "0 0 4 * * *"
		},
		"a manual trigger": func(p *harbor.ReplicationPolicy, _ namespaceObjects) {
			p.Trigger = &harbor.ReplicationTrigger{Type: "manual"}
		},
		"no trigger": func(p *harbor.ReplicationPolicy, _ namespaceObjects) {
			p.Trigger = nil
		},
		"a remote destination": func(p *harbor.ReplicationPolicy, _ namespaceObjects) {
			p.DestRegistry = &harbor.Registry{ID: 3, Name: "hub"}
		},
		"a namespace that does not see the project": func(_ *harbor.ReplicationPolicy, n namespaceObjects) {
			delete(n, "ns1")
		},
		"a recreated namespace": func(_ *harbor.ReplicationPolicy, n namespaceObjects) {
			n["ns1"] = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns1", UID: "new"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, n := storedPolicy(), namespaceObjects{"ns1": namespace("ns1")}
			change(p, n)
			if d, ok := replicationConfig.visible(p, n); ok {
				t.Errorf("visible as %+v", d)
			}
		})
	}
}

func TestReplicationObject(t *testing.T) {
	start := created.Add(time.Minute)
	e := &harbor.ReplicationExecution{
		ID: 42, PolicyID: 7, Status: "Failed", StatusText: "1 of 5 tasks failed", Trigger: "scheduled",
		StartTime: start, Total: 5, Failed: 1, Succeed: 2, InProgress: 1, Stopped: 1,
	}
	p := storedPolicy()
	d, _ := replicationConfig.visible(p, namespaceObjects{"ns1": namespace("ns1")})
	got := replicationObject(p, d, e)

	want := nginxReplication()
	want.Generation = 1
	want.CreationTimestamp = metav1.NewTime(created)
	want.Status = v1alpha1.HarborReplicationStatus{
		Destination: "proj/k8s/ns1/nginx",
		LastExecution: &v1alpha1.HarborReplicationExecution{
			ID: 42, Trigger: v1alpha1.ReplicationTriggerScheduled, Phase: v1alpha1.ReplicationPhaseFailed, Message: "1 of 5 tasks failed",
			StartTime: &metav1.Time{Time: start}, Total: 5, Succeeded: 2, Failed: 1, InProgress: 1, Stopped: 1,
		},
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	want.ResourceVersion = hex.EncodeToString(sum[:])[:16]
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}

	e.EndTime = start.Add(time.Minute)
	if after := replicationObject(p, d, e); after.ResourceVersion == got.ResourceVersion {
		t.Error("the resourceVersion did not change with the status")
	}
	if never := replicationObject(p, d, nil); never.Status.LastExecution != nil {
		t.Errorf("a replication that never ran has execution %+v", never.Status.LastExecution)
	}
}

func TestReplicationPhase(t *testing.T) {
	for status, want := range map[string]v1alpha1.HarborReplicationPhase{
		"InProgress": v1alpha1.ReplicationPhaseInProgress,
		"Succeed":    v1alpha1.ReplicationPhaseSucceeded,
		"Failed":     v1alpha1.ReplicationPhaseFailed,
		"Stopped":    v1alpha1.ReplicationPhaseStopped,
		"Running":    v1alpha1.ReplicationPhaseUnknown,
		"":           v1alpha1.ReplicationPhaseUnknown,
	} {
		if got := replicationExecution(&harbor.ReplicationExecution{Status: status}).Phase; got != want {
			t.Errorf("status %q is phase %q, want %q", status, got, want)
		}
	}
}

func TestReplicationTrigger(t *testing.T) {
	for trigger, want := range map[string]v1alpha1.HarborReplicationTrigger{
		"manual":      v1alpha1.ReplicationTriggerManual,
		"scheduled":   v1alpha1.ReplicationTriggerScheduled,
		"event_based": v1alpha1.ReplicationTriggerUnknown,
		"":            v1alpha1.ReplicationTriggerUnknown,
	} {
		if got := replicationExecution(&harbor.ReplicationExecution{Trigger: trigger}).Trigger; got != want {
			t.Errorf("trigger %q is %q, want %q", trigger, got, want)
		}
	}
}
