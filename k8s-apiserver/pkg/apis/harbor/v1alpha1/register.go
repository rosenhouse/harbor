package v1alpha1

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const GroupName = "harbor.goharbor.io"

var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

func AddToScheme(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&HarborRepository{},
		&HarborRepositoryList{},
		&HarborArtifact{},
		&HarborArtifactList{},
	)
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return scheme.AddFieldLabelConversionFunc(SchemeGroupVersion.WithKind("HarborArtifact"), artifactFieldLabel)
}

func artifactFieldLabel(label, value string) (string, string, error) {
	switch label {
	case "metadata.name", "metadata.namespace", "status.repository":
		return label, value, nil
	}
	return "", "", fmt.Errorf("field label not supported: %s", label)
}
