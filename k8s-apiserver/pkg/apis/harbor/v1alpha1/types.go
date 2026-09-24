package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// HarborRepository is a Harbor repository in the project that the namespace is labeled with.
// Its name is the repository's name within the project with slashes replaced by dots.
// A repository name that contains dots, or whose dotted form is not a valid name or ends in "-" and 10 hex digits,
// instead gets a sanitized name with a suffix of "-" and 10 hex digits from a hash.
type HarborRepository struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata.
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Status is the repository as observed in Harbor.
	Status HarborRepositoryStatus `json:"status,omitempty"`
}

// HarborRepositoryStatus is the repository as observed in Harbor.
type HarborRepositoryStatus struct {
	// Name is the repository's full name in Harbor, starting with the project.
	Name string `json:"name"`
	// +optional
	Description   string `json:"description,omitempty"`
	ArtifactCount int64  `json:"artifactCount"`
	// PullCount is the number of pulls of all artifacts in the repository.
	PullCount int64 `json:"pullCount"`
	// UpdateTime is when Harbor last changed the repository.
	// +optional
	UpdateTime *metav1.Time `json:"updateTime,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// HarborRepositoryList is a list of HarborRepository.
type HarborRepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata.
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []HarborRepository `json:"items"`
}

// RepositoryLabel holds the name of an artifact's HarborRepository, if that name fits in a label value.
const RepositoryLabel = GroupName + "/repository"

// ReplicationLabel on a replicated HarborArtifact names the HarborReplication that copied it.
const ReplicationLabel = GroupName + "/replication"

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// HarborArtifact is an artifact in a Harbor repository in the project that the namespace is labeled with.
// Its name is the HarborRepository name followed by the digest's algorithm and first 12 hex digits, such as team.api.sha256-0123456789ab.
// A name that would exceed 253 characters has its repository part shortened to end in a hash.
// Of artifacts in a repository that would share a name, only the one that Harbor created first appears.
// The harbor.goharbor.io/repository label holds the HarborRepository name if it fits in a label value.
// In the namespace of the HarborReplication that copied it, an artifact has the harbor.goharbor.io/replication label
// and an ownerReference to the HarborReplication.
type HarborArtifact struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata.
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Status is the artifact as observed in Harbor.
	Status HarborArtifactStatus `json:"status,omitempty"`
}

// HarborArtifactStatus is the artifact as observed in Harbor.
type HarborArtifactStatus struct {
	// Repository is the full name of the artifact's repository in Harbor, starting with the project.
	Repository string `json:"repository"`
	// Digest is the manifest's digest.
	Digest string `json:"digest"`
	// Type is Harbor's classification, such as IMAGE or CHART.
	// +optional
	Type string `json:"type,omitempty"`
	// MediaType is the media type of the manifest.
	// +optional
	MediaType string `json:"mediaType,omitempty"`
	// ConfigMediaType is the media type of the manifest's config. An index has none.
	// +optional
	ConfigMediaType string `json:"configMediaType,omitempty"`
	// ArtifactType is the artifactType field of an OCI manifest.
	// +optional
	ArtifactType string `json:"artifactType,omitempty"`
	// Size is the size in bytes, including referenced blobs.
	Size int64 `json:"size"`
	// +optional
	// +listType=map
	// +listMapKey=name
	Tags []HarborTag `json:"tags,omitempty"`
	// PushTime is when the artifact was first pushed.
	// +optional
	PushTime *metav1.Time `json:"pushTime,omitempty"`
	// PullTime is when the artifact was last pulled.
	// +optional
	PullTime *metav1.Time `json:"pullTime,omitempty"`
	// Annotations are the manifest's OCI annotations.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
	// References are the manifests that an index refers to.
	// +optional
	// +listType=atomic
	References []HarborArtifactReference `json:"references,omitempty"`
}

// HarborTag is a tag on an artifact.
type HarborTag struct {
	Name string `json:"name"`
	// PushTime is when the tag was pushed to this artifact.
	// +optional
	PushTime *metav1.Time `json:"pushTime,omitempty"`
	// PullTime is when the artifact was last pulled by this tag.
	// +optional
	PullTime *metav1.Time `json:"pullTime,omitempty"`
}

// HarborArtifactReference is a manifest that an index refers to.
type HarborArtifactReference struct {
	// Digest is the referenced manifest's digest.
	Digest string `json:"digest"`
	// Platform is the platform that the referenced manifest is for.
	// +optional
	Platform *HarborPlatform `json:"platform,omitempty"`
}

// HarborPlatform is the platform that an image runs on.
type HarborPlatform struct {
	// Architecture is the CPU architecture, such as amd64 or arm64.
	Architecture string `json:"architecture"`
	// OS is the operating system, such as linux.
	OS string `json:"os"`
	// Variant is the CPU variant, such as v8 for arm64.
	// +optional
	Variant string `json:"variant,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// HarborArtifactList is a list of HarborArtifact.
type HarborArtifactList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata.
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []HarborArtifact `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// HarborReplication copies artifacts from a remote registry into the project that the namespace is labeled with.
// It is a Harbor replication policy in pull mode. It runs once when created, and then on its schedule.
// Updates cannot change it. Delete and recreate it to change it.
type HarborReplication struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata.
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is what to copy, and when.
	Spec HarborReplicationSpec `json:"spec"`
	// Status is the replication as observed in Harbor.
	// +optional
	Status HarborReplicationStatus `json:"status,omitempty"`
}

// HarborReplicationSpec is what to copy, and when.
type HarborReplicationSpec struct {
	// Registry is the name of a Harbor registry endpoint that the server allows replications from.
	Registry string `json:"registry"`
	// Repository is the path of the source repository, such as library/nginx. It takes no glob.
	Repository string `json:"repository"`
	// Tag is a Harbor tag filter, a glob such as 1.27*. Use * to copy every tag.
	// It has at most two * and one {} group, so that Harbor matches it quickly.
	Tag string `json:"tag"`
	// Schedule is a Harbor cron expression that runs the replication again, such as "0 0 3 * * *".
	// Harbor runs it in UTC. Its first field is seconds, which must be 0. Minutes must be a single number.
	// +optional
	Schedule string `json:"schedule,omitempty"`
}

// HarborReplicationStatus is the replication as observed in Harbor.
type HarborReplicationStatus struct {
	// Destination is where the copies go in Harbor, starting with the project.
	// A source repository library/nginx lands in <destination>/library/nginx.
	// +optional
	Destination string `json:"destination,omitempty"`
	// LastExecution is the newest run that Harbor did not skip. It is empty until the first run starts.
	// +optional
	LastExecution *HarborReplicationExecution `json:"lastExecution,omitempty"`
}

// HarborReplicationExecution is one run of a replication.
// Each of its tasks copies one source repository.
type HarborReplicationExecution struct {
	// ID is Harbor's ID for the execution.
	ID int64 `json:"id"`
	// Trigger is what started the run.
	Trigger HarborReplicationTrigger `json:"trigger"`
	// Phase is the state of the run.
	Phase HarborReplicationPhase `json:"phase"`
	// Message is Harbor's status text, such as why the run failed.
	// +optional
	Message string `json:"message,omitempty"`
	// StartTime is when the run started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// EndTime is when the run ended.
	// +optional
	EndTime *metav1.Time `json:"endTime,omitempty"`
	// Total is the number of tasks.
	Total int64 `json:"total"`
	// Succeeded is the number of tasks that succeeded.
	Succeeded int64 `json:"succeeded"`
	// Failed is the number of tasks that failed.
	Failed int64 `json:"failed"`
	// InProgress is the number of tasks that are pending or running.
	InProgress int64 `json:"inProgress"`
	// Stopped is the number of tasks that were stopped.
	Stopped int64 `json:"stopped"`
}

// HarborReplicationTrigger is what started an execution.
// +enum
type HarborReplicationTrigger string

const (
	// ReplicationTriggerManual is a run started on request, such as the run when the replication is created.
	ReplicationTriggerManual HarborReplicationTrigger = "Manual"
	// ReplicationTriggerScheduled is a run on the schedule.
	ReplicationTriggerScheduled HarborReplicationTrigger = "Scheduled"
	// ReplicationTriggerUnknown is a trigger that the server doesn't recognize.
	ReplicationTriggerUnknown HarborReplicationTrigger = "Unknown"
)

// HarborReplicationPhase is the state of an execution.
// +enum
type HarborReplicationPhase string

const (
	ReplicationPhaseInProgress HarborReplicationPhase = "InProgress"
	ReplicationPhaseSucceeded  HarborReplicationPhase = "Succeeded"
	ReplicationPhaseFailed     HarborReplicationPhase = "Failed"
	ReplicationPhaseStopped    HarborReplicationPhase = "Stopped"
	// ReplicationPhaseUnknown is a state that the server doesn't recognize.
	ReplicationPhaseUnknown HarborReplicationPhase = "Unknown"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// HarborReplicationList is a list of HarborReplication.
type HarborReplicationList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata.
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []HarborReplication `json:"items"`
}
