package registry

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

// ReplicationConfig maps HarborReplications to Harbor replication policies.
type ReplicationConfig struct {
	Project string
	// Prefix starts each policy name, and is the path segment in the project that replications copy into.
	Prefix string
	// Registries are the names of the registry endpoints that replications may copy from.
	Registries []string
}

const managedBy = "harbor-apiserver"

// policyDescription is the description of a policy: its HarborReplication without the status.
type policyDescription struct {
	ManagedBy    string                         `json:"managedBy"`
	Namespace    string                         `json:"namespace"`
	NamespaceUID types.UID                      `json:"namespaceUID"`
	Name         string                         `json:"name"`
	UID          types.UID                      `json:"uid"`
	Labels       map[string]string              `json:"labels,omitempty"`
	Annotations  map[string]string              `json:"annotations,omitempty"`
	Spec         v1alpha1.HarborReplicationSpec `json:"spec"`
}

func describe(obj *v1alpha1.HarborReplication, namespaceUID types.UID) *policyDescription {
	return &policyDescription{
		ManagedBy:    managedBy,
		Namespace:    obj.Namespace,
		NamespaceUID: namespaceUID,
		Name:         obj.Name,
		UID:          obj.UID,
		Labels:       obj.Labels,
		Annotations:  obj.Annotations,
		Spec:         obj.Spec,
	}
}

// policyPrefix starts the name of every policy of the project.
func (c ReplicationConfig) policyPrefix() string {
	return c.Prefix + "." + c.Project + "."
}

// namespacePolicyPrefix starts the name of every policy of a namespace.
func (c ReplicationConfig) namespacePolicyPrefix(namespace string) string {
	return c.policyPrefix() + namespace + "."
}

func (c ReplicationConfig) policyName(namespace, name string) string {
	return c.namespacePolicyPrefix(namespace) + name
}

// destination is where a replication copies to. A source repository keeps its path under it.
func (c ReplicationConfig) destination(namespace, name string) string {
	return c.Project + "/" + c.Prefix + "/" + namespace + "/" + name
}

// repositoryPrefix starts the name, within the project, of each repository that a replication copies into.
func (c ReplicationConfig) repositoryPrefix(namespace, name string) string {
	return c.Prefix + "/" + namespace + "/" + name + "/"
}

// policy returns the policy to create for a description, pulling from the registry endpoint with registryID.
func (c ReplicationConfig) policy(d *policyDescription, registryID int64) *harbor.ReplicationPolicy {
	// Marshal fails only on types that cannot be JSON.
	description, _ := json.Marshal(d)
	return &harbor.ReplicationPolicy{
		Name:                      c.policyName(d.Namespace, d.Name),
		Description:               string(description),
		SrcRegistry:               &harbor.Registry{ID: registryID},
		DestNamespace:             c.destination(d.Namespace, d.Name),
		DestNamespaceReplaceCount: ptr.To[int8](0),
		Trigger:                   policyTrigger(d.Spec),
		Filters:                   policyFilters(d.Spec),
		Override:                  true,
		Enabled:                   true,
		SingleActiveReplication:   true,
	}
}

func policyTrigger(spec v1alpha1.HarborReplicationSpec) *harbor.ReplicationTrigger {
	if spec.Schedule == "" {
		return &harbor.ReplicationTrigger{Type: "manual"}
	}
	return &harbor.ReplicationTrigger{Type: "scheduled", Settings: &harbor.ReplicationTriggerSettings{Cron: spec.Schedule}}
}

func policyFilters(spec v1alpha1.HarborReplicationSpec) []harbor.ReplicationFilter {
	return []harbor.ReplicationFilter{
		{Type: "name", Value: spec.Repository},
		{Type: "tag", Value: spec.Tag, Decoration: "matches"},
	}
}

// visible returns the description of a policy that the server shows.
// The server shows a policy only if the policy copies what its description says, to where the server would copy it,
// and the described namespace sees the project and has the UID that the replication was created in.
func (c ReplicationConfig) visible(p *harbor.ReplicationPolicy, n Namespaces) (*policyDescription, bool) {
	var d policyDescription
	if json.Unmarshal([]byte(p.Description), &d) != nil || d.ManagedBy != managedBy {
		return nil, false
	}
	switch {
	case p.Name != c.policyName(d.Namespace, d.Name),
		len(validation.IsDNS1123Label(d.Name)) > 0,
		p.DestNamespace != c.destination(d.Namespace, d.Name),
		p.DestNamespaceReplaceCount == nil || *p.DestNamespaceReplaceCount != 0,
		p.SrcRegistry == nil || p.SrcRegistry.ID == 0 || p.SrcRegistry.Name != d.Spec.Registry || !slices.Contains(c.Registries, d.Spec.Registry),
		p.DestRegistry != nil && p.DestRegistry.ID != 0,
		!reflect.DeepEqual(p.Filters, policyFilters(d.Spec)),
		!reflect.DeepEqual(p.Trigger, policyTrigger(d.Spec)):
		return nil, false
	}
	ns, ok := n.Namespace(d.Namespace)
	if !ok || ns.UID != d.NamespaceUID {
		return nil, false
	}
	return &d, true
}

// replicationObject renders a visible policy with its description and latest execution, which may be nil.
func replicationObject(p *harbor.ReplicationPolicy, d *policyDescription, e *harbor.ReplicationExecution) *v1alpha1.HarborReplication {
	obj := &v1alpha1.HarborReplication{
		ObjectMeta: metav1.ObjectMeta{
			Name:              d.Name,
			Namespace:         d.Namespace,
			UID:               d.UID,
			Generation:        1,
			CreationTimestamp: metav1.NewTime(p.CreationTime),
			Labels:            d.Labels,
			Annotations:       d.Annotations,
		},
		Spec:   d.Spec,
		Status: v1alpha1.HarborReplicationStatus{Destination: p.DestNamespace},
	}
	if e != nil {
		obj.Status.LastExecution = replicationExecution(e)
	}
	b, _ := json.Marshal(obj)
	sum := sha256.Sum256(b)
	obj.ResourceVersion = hex.EncodeToString(sum[:])[:16]
	return obj
}

// replicationPhases maps Harbor's execution statuses to phases.
var replicationPhases = map[string]v1alpha1.HarborReplicationPhase{
	"InProgress": v1alpha1.ReplicationPhaseInProgress,
	"Succeed":    v1alpha1.ReplicationPhaseSucceeded,
	"Failed":     v1alpha1.ReplicationPhaseFailed,
	"Stopped":    v1alpha1.ReplicationPhaseStopped,
}

// replicationTriggers maps Harbor's execution triggers to triggers.
var replicationTriggers = map[string]v1alpha1.HarborReplicationTrigger{
	"manual":    v1alpha1.ReplicationTriggerManual,
	"scheduled": v1alpha1.ReplicationTriggerScheduled,
}

func replicationExecution(e *harbor.ReplicationExecution) *v1alpha1.HarborReplicationExecution {
	return &v1alpha1.HarborReplicationExecution{
		ID:         e.ID,
		Trigger:    cmp.Or(replicationTriggers[e.Trigger], v1alpha1.ReplicationTriggerUnknown),
		Phase:      cmp.Or(replicationPhases[e.Status], v1alpha1.ReplicationPhaseUnknown),
		Message:    e.StatusText,
		StartTime:  optionalTime(e.StartTime),
		EndTime:    optionalTime(e.EndTime),
		Total:      e.Total,
		Succeeded:  e.Succeed,
		Failed:     e.Failed,
		InProgress: e.InProgress,
		Stopped:    e.Stopped,
	}
}
