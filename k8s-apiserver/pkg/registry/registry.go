// Package registry serves Harbor data as API resources.
package registry

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/meta"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/duration"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

// Harbor reads a Harbor project.
type Harbor interface {
	ListRepositories(ctx context.Context, project string) ([]harbor.Repository, error)
	ListArtifacts(ctx context.Context, project, repository string) ([]harbor.Artifact, error)
}

// Namespaces decides which namespaces see the project.
type Namespaces interface {
	Allows(namespace string) bool
	Namespaces() []string
}

type object interface {
	runtime.Object
	metav1.Object
}

// storage serves one resource from a Store.
// It ignores resourceVersion, since it serves only the store's current items, and its objects have none.
type storage struct {
	store     *Store
	resource  schema.GroupResource
	newObject func() runtime.Object
	newList   func() runtime.Object
	// fields returns an object's selectable fields besides its name and namespace.
	fields func(runtime.Object) fields.Set
	// mayReturnArtifacts returns whether a request with opts could return artifacts of repository in project. Nil means never.
	mayReturnArtifacts func(opts *metainternalversion.ListOptions, project, repository string) bool
}

func (s *storage) New() runtime.Object     { return s.newObject() }
func (s *storage) NewList() runtime.Object { return s.newList() }
func (s *storage) Destroy()                {}
func (s *storage) NamespaceScoped() bool   { return true }

func (s *storage) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	byName := &metainternalversion.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", name)}
	return s.store.get(s.resource, genericapirequest.NamespaceValue(ctx), name, s.needs(byName))
}

func (s *storage) List(ctx context.Context, opts *metainternalversion.ListOptions) (runtime.Object, error) {
	objs, err := s.store.list(s.resource, genericapirequest.NamespaceValue(ctx), s.matcher(opts), s.needs(opts))
	if err != nil {
		return nil, err
	}
	list := s.newList()
	if err := meta.SetList(list, objs); err != nil {
		return nil, err
	}
	return list, nil
}

// needs returns whether a response to a request with opts needs a repository's artifacts.
func (s *storage) needs(opts *metainternalversion.ListOptions) func(repository string) bool {
	return func(repository string) bool {
		return s.mayReturnArtifacts != nil && s.mayReturnArtifacts(opts, s.store.project, repository)
	}
}

// matcher returns whether an object, in a namespace, matches the selectors of opts.
func (s *storage) matcher(opts *metainternalversion.ListOptions) func(object, string) bool {
	return func(o object, namespace string) bool {
		if opts == nil {
			return true
		}
		if opts.LabelSelector != nil && !opts.LabelSelector.Matches(labels.Set(o.GetLabels())) {
			return false
		}
		if opts.FieldSelector == nil {
			return true
		}
		set := fields.Set{"metadata.name": o.GetName(), "metadata.namespace": namespace}
		if s.fields != nil {
			maps.Copy(set, s.fields(o))
		}
		return opts.FieldSelector.Matches(set)
	}
}

// uid is stable for a Harbor object seen from a namespace, and changes if Harbor recreates the object.
func uid(namespace, resource string, harborID int64) types.UID {
	return types.UID(uuid.NewSHA1(uuid.NameSpaceURL, fmt.Appendf(nil, "harbor.goharbor.io/%s/%s/%d", namespace, resource, harborID)).String())
}

func optionalTime(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	return &metav1.Time{Time: t}
}

func age(t metav1.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	return duration.HumanDuration(time.Since(t.Time))
}

// errorKind describes why a poll failed, without Harbor's address or response.
func errorKind(err error) error {
	for _, kind := range []error{errSlowRead, harbor.ErrUnavailable, harbor.ErrUnauthorized, harbor.ErrForbidden, harbor.ErrNotFound} {
		if errors.Is(err, kind) {
			return kind
		}
	}
	return errors.New("unexpected error reading harbor")
}
