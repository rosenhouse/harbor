// Package registry serves Harbor data as API resources.
package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/duration"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/klog/v2"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

// Harbor reads a Harbor project.
type Harbor interface {
	ListRepositories(ctx context.Context, project string) ([]harbor.Repository, error)
	GetRepository(ctx context.Context, project, repository string) (*harbor.Repository, error)
}

// Namespaces decides which namespaces see the project.
type Namespaces interface {
	Allows(namespace string) bool
	Namespaces() []string
}

// requestNamespaces returns the allowed namespaces that the request covers.
func requestNamespaces(ctx context.Context, n Namespaces) []string {
	ns := genericapirequest.NamespaceValue(ctx)
	switch {
	case ns == "":
		return n.Namespaces()
	case n.Allows(ns):
		return []string{ns}
	}
	return nil
}

func matches(meta metav1.ObjectMeta, opts *metainternalversion.ListOptions) bool {
	if opts == nil {
		return true
	}
	if opts.LabelSelector != nil && !opts.LabelSelector.Matches(labels.Set(meta.Labels)) {
		return false
	}
	return opts.FieldSelector == nil || opts.FieldSelector.Matches(fields.Set{
		"metadata.name":      meta.Name,
		"metadata.namespace": meta.Namespace,
	})
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

// harborError logs a failed Harbor request and reports it without Harbor's address or response.
// A missing project or a rejected robot account is a server misconfiguration.
func harborError(err error) error {
	klog.ErrorS(err, "Harbor request failed")
	if errors.Is(err, harbor.ErrUnavailable) {
		return apierrors.NewServiceUnavailable(harbor.ErrUnavailable.Error())
	}
	for _, kind := range []error{harbor.ErrUnauthorized, harbor.ErrForbidden, harbor.ErrNotFound} {
		if errors.Is(err, kind) {
			return apierrors.NewInternalError(kind)
		}
	}
	return apierrors.NewInternalError(errors.New("unexpected error reading harbor"))
}
