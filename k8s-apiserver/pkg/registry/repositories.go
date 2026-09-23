package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

var repositoriesResource = v1alpha1.Resource("harborrepositories")

// Repositories serves HarborRepository objects.
type Repositories struct {
	harbor     Harbor
	project    string
	namespaces Namespaces
}

var (
	_ rest.Getter               = &Repositories{}
	_ rest.Lister               = &Repositories{}
	_ rest.Scoper               = &Repositories{}
	_ rest.SingularNameProvider = &Repositories{}
)

func NewRepositories(h Harbor, project string, n Namespaces) *Repositories {
	return &Repositories{harbor: h, project: project, namespaces: n}
}

func (r *Repositories) New() runtime.Object     { return &v1alpha1.HarborRepository{} }
func (r *Repositories) NewList() runtime.Object { return &v1alpha1.HarborRepositoryList{} }
func (r *Repositories) Destroy()                {}
func (r *Repositories) NamespaceScoped() bool   { return true }
func (r *Repositories) GetSingularName() string { return "harborrepository" }

func (r *Repositories) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	ns := genericapirequest.NamespaceValue(ctx)
	if !r.namespaces.Allows(ns) {
		return nil, apierrors.NewNotFound(repositoriesResource, name)
	}

	switch {
	case len(validation.IsDNS1123Subdomain(name)) > 0:
	case isHashed(name):
		repos, err := r.harbor.ListRepositories(ctx, r.project)
		if err != nil {
			return nil, harborError(err)
		}
		for _, repo := range repos {
			if r.objectName(repo) == name {
				return r.object(ns, repo), nil
			}
		}
	default:
		repo, err := r.harbor.GetRepository(ctx, r.project, repositoryNameCandidate(name))
		if err == nil {
			return r.object(ns, *repo), nil
		}
		if !errors.Is(err, harbor.ErrNotFound) {
			return nil, harborError(err)
		}
	}
	return nil, apierrors.NewNotFound(repositoriesResource, name)
}

func (r *Repositories) List(ctx context.Context, opts *metainternalversion.ListOptions) (runtime.Object, error) {
	list := &v1alpha1.HarborRepositoryList{}
	namespaces := requestNamespaces(ctx, r.namespaces)
	if len(namespaces) == 0 {
		return list, nil
	}
	repos, err := r.harbor.ListRepositories(ctx, r.project)
	if err != nil {
		return nil, harborError(err)
	}
	for _, ns := range namespaces {
		for _, repo := range repos {
			if obj := r.object(ns, repo); matches(obj.ObjectMeta, opts) {
				list.Items = append(list.Items, *obj)
			}
		}
	}
	return list, nil
}

func (r *Repositories) objectName(repo harbor.Repository) string {
	return repositoryObjectName(strings.TrimPrefix(repo.Name, r.project+"/"))
}

func (r *Repositories) object(namespace string, repo harbor.Repository) *v1alpha1.HarborRepository {
	return &v1alpha1.HarborRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:              r.objectName(repo),
			Namespace:         namespace,
			UID:               uid(namespace, repositoriesResource.Resource, repo.ID),
			CreationTimestamp: metav1.NewTime(repo.CreationTime),
		},
		Status: v1alpha1.HarborRepositoryStatus{
			Name:          repo.Name,
			Description:   repo.Description,
			ArtifactCount: repo.ArtifactCount,
			PullCount:     repo.PullCount,
			UpdateTime:    optionalTime(repo.UpdateTime),
		},
	}
}

var repositoryColumns = []metav1.TableColumnDefinition{
	{Name: "Name", Type: "string", Format: "name"},
	{Name: "Repository", Type: "string", Priority: 1, Description: "Full name in Harbor"},
	{Name: "Artifacts", Type: "integer"},
	{Name: "Pulls", Type: "integer"},
	{Name: "Age", Type: "string"},
}

func (r *Repositories) ConvertToTable(_ context.Context, obj runtime.Object, _ runtime.Object) (*metav1.Table, error) {
	table := &metav1.Table{ColumnDefinitions: repositoryColumns, Rows: []metav1.TableRow{}}
	add := func(o *v1alpha1.HarborRepository) {
		table.Rows = append(table.Rows, metav1.TableRow{
			Cells:  []any{o.Name, o.Status.Name, o.Status.ArtifactCount, o.Status.PullCount, age(o.CreationTimestamp)},
			Object: runtime.RawExtension{Object: o},
		})
	}
	switch o := obj.(type) {
	case *v1alpha1.HarborRepository:
		add(o)
	case *v1alpha1.HarborRepositoryList:
		table.ListMeta = o.ListMeta
		for i := range o.Items {
			add(&o.Items[i])
		}
	default:
		return nil, fmt.Errorf("cannot convert %T to a table", obj)
	}
	return table, nil
}
