// Package apiserver assembles the harbor.goharbor.io API server.
package apiserver

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/endpoints/openapi"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/util/compatibility"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
	generatedopenapi "github.com/rosenhouse/harbor/k8s-apiserver/pkg/generated/openapi"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/registry"
)

var (
	Scheme = runtime.NewScheme()
	Codecs = serializer.NewCodecFactory(Scheme)
)

func init() {
	utilruntime.Must(v1alpha1.AddToScheme(Scheme))
	metav1.AddToGroupVersion(Scheme, schema.GroupVersion{Version: "v1"})
}

// NewConfig returns a server config without serving, authentication, or authorization.
func NewConfig() *genericapiserver.Config {
	c := genericapiserver.NewConfig(Codecs)
	namer := openapi.NewDefinitionNamer(Scheme)
	c.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(generatedopenapi.GetOpenAPIDefinitions, namer)
	c.OpenAPIConfig.Info.Title = "Harbor"
	c.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(generatedopenapi.GetOpenAPIDefinitions, namer)
	c.OpenAPIV3Config.Info.Title = "Harbor"
	c.EffectiveVersion = compatibility.DefaultBuildEffectiveVersion()
	return c
}

func New(c genericapiserver.CompletedConfig) (*genericapiserver.GenericAPIServer, error) {
	s, err := c.New("harbor-apiserver", genericapiserver.NewEmptyDelegate())
	if err != nil {
		return nil, err
	}

	group := genericapiserver.NewDefaultAPIGroupInfo(v1alpha1.GroupName, Scheme, metav1.ParameterCodec, Codecs)
	group.VersionedResourcesStorageMap[v1alpha1.SchemeGroupVersion.Version] = map[string]rest.Storage{
		"harborrepositories": registry.NewStub(v1alpha1.Resource("harborrepositories"), "harborrepository",
			func() runtime.Object { return &v1alpha1.HarborRepository{} },
			func() runtime.Object { return &v1alpha1.HarborRepositoryList{} }),
		"harborartifacts": registry.NewStub(v1alpha1.Resource("harborartifacts"), "harborartifact",
			func() runtime.Object { return &v1alpha1.HarborArtifact{} },
			func() runtime.Object { return &v1alpha1.HarborArtifactList{} }),
	}
	if err := s.InstallAPIGroup(&group); err != nil {
		return nil, err
	}
	return s, nil
}
