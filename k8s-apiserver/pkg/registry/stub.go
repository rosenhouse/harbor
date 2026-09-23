package registry

import (
	"context"

	"k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"
)

// Stub serves a namespaced resource that has no objects.
type Stub struct {
	rest.TableConvertor
	resource schema.GroupResource
	singular string
	newObj   func() runtime.Object
	newList  func() runtime.Object
}

var (
	_ rest.Getter               = &Stub{}
	_ rest.Lister               = &Stub{}
	_ rest.Scoper               = &Stub{}
	_ rest.SingularNameProvider = &Stub{}
)

func NewStub(resource schema.GroupResource, singular string, newObj, newList func() runtime.Object) *Stub {
	return &Stub{
		TableConvertor: rest.NewDefaultTableConvertor(resource),
		resource:       resource,
		singular:       singular,
		newObj:         newObj,
		newList:        newList,
	}
}

func (s *Stub) New() runtime.Object     { return s.newObj() }
func (s *Stub) NewList() runtime.Object { return s.newList() }
func (s *Stub) Destroy()                {}
func (s *Stub) NamespaceScoped() bool   { return true }
func (s *Stub) GetSingularName() string { return s.singular }

func (s *Stub) Get(_ context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	return nil, errors.NewNotFound(s.resource, name)
}

func (s *Stub) List(context.Context, *metainternalversion.ListOptions) (runtime.Object, error) {
	return s.newList(), nil
}
