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

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// HarborArtifact is an artifact in a Harbor repository in the project that the namespace is labeled with.
// Its name is the HarborRepository name followed by the digest's algorithm and first 12 hex digits, such as team.api.sha256-0123456789ab.
// A name that would exceed 253 characters has its repository part shortened to end in a hash.
// Of artifacts in a repository that would share a name, only the one that Harbor created first appears.
// The harbor.goharbor.io/repository label holds the HarborRepository name if it fits in a label value.
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
