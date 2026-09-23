package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const GroupName = "harbor.goharbor.io"

var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

// AddToScheme registers the types as both v1alpha1 and the internal version.
// With a single served version, the internal version needs no separate types.
func AddToScheme(scheme *runtime.Scheme) error {
	internal := schema.GroupVersion{Group: GroupName, Version: runtime.APIVersionInternal}
	for _, gv := range []schema.GroupVersion{SchemeGroupVersion, internal} {
		scheme.AddKnownTypes(gv,
			&HarborRepository{},
			&HarborRepositoryList{},
			&HarborArtifact{},
			&HarborArtifactList{},
		)
	}
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}
