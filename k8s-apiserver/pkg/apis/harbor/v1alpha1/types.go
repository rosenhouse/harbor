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

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// HarborArtifact is an artifact in a Harbor repository in the project that the namespace is labeled with.
type HarborArtifact struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata.
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Status is the artifact as observed in Harbor.
	Status HarborArtifactStatus `json:"status,omitempty"`
}

// HarborArtifactStatus is the artifact as observed in Harbor.
type HarborArtifactStatus struct{}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// HarborArtifactList is a list of HarborArtifact.
type HarborArtifactList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata.
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []HarborArtifact `json:"items"`
}
