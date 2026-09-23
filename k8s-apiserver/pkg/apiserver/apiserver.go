// Package apiserver assembles the harbor.goharbor.io API server.
package apiserver

import (
	"slices"

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
	scheme = runtime.NewScheme()
	codecs = serializer.NewCodecFactory(scheme)
)

func init() {
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	metav1.AddToGroupVersion(scheme, schema.GroupVersion{Version: "v1"})
}

// withoutProtobuf stops clients from negotiating protobuf, which the types don't implement.
type withoutProtobuf struct {
	runtime.NegotiatedSerializer
}

func (w withoutProtobuf) SupportedMediaTypes() []runtime.SerializerInfo {
	return slices.DeleteFunc(slices.Clone(w.NegotiatedSerializer.SupportedMediaTypes()), func(i runtime.SerializerInfo) bool {
		return i.MediaType == runtime.ContentTypeProtobuf
	})
}

// NewConfig returns a server config without serving, authentication, or authorization.
func NewConfig() *genericapiserver.Config {
	c := genericapiserver.NewConfig(codecs)
	namer := openapi.NewDefinitionNamer(scheme)
	c.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(generatedopenapi.GetOpenAPIDefinitions, namer)
	c.OpenAPIConfig.Info.Title = "Harbor"
	c.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(generatedopenapi.GetOpenAPIDefinitions, namer)
	c.OpenAPIV3Config.Info.Title = "Harbor"
	c.EffectiveVersion = compatibility.DefaultBuildEffectiveVersion()
	return c
}

// New serves Harbor project data from s.
func New(c genericapiserver.CompletedConfig, s *registry.Store) (*genericapiserver.GenericAPIServer, error) {
	server, err := c.New("harbor-apiserver", genericapiserver.NewEmptyDelegate())
	if err != nil {
		return nil, err
	}

	group := genericapiserver.NewDefaultAPIGroupInfo(v1alpha1.GroupName, scheme, metav1.ParameterCodec, codecs)
	group.NegotiatedSerializer = withoutProtobuf{group.NegotiatedSerializer}
	group.VersionedResourcesStorageMap[v1alpha1.SchemeGroupVersion.Version] = map[string]rest.Storage{
		"harborrepositories": registry.NewRepositories(s),
		"harborartifacts":    registry.NewArtifacts(s),
	}
	if err := server.InstallAPIGroup(&group); err != nil {
		return nil, err
	}
	return server, nil
}
