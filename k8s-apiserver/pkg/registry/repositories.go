package registry

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

var repositoriesResource = v1alpha1.Resource("harborrepositories")

// Repositories serves HarborRepository objects.
type Repositories struct {
	storage
}

var (
	_ rest.Getter               = &Repositories{}
	_ rest.Lister               = &Repositories{}
	_ rest.Watcher              = &Repositories{}
	_ rest.Scoper               = &Repositories{}
	_ rest.SingularNameProvider = &Repositories{}
)

func NewRepositories(s *Store) *Repositories {
	return &Repositories{storage{
		store:     s,
		resource:  repositoriesResource,
		newObject: func() runtime.Object { return &v1alpha1.HarborRepository{} },
		newList:   func() runtime.Object { return &v1alpha1.HarborRepositoryList{} },
	}}
}

func (r *Repositories) GetSingularName() string { return "harborrepository" }

// repositoryItem returns the item of repo, which is named within its project.
func repositoryItem(name string, repo harbor.Repository) *item {
	return &item{harborID: repo.ID, obj: &v1alpha1.HarborRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:              repositoryObjectName(name),
			CreationTimestamp: metav1.NewTime(repo.CreationTime),
		},
		Status: v1alpha1.HarborRepositoryStatus{
			Name:          repo.Name,
			Description:   repo.Description,
			ArtifactCount: repo.ArtifactCount,
			PullCount:     repo.PullCount,
			UpdateTime:    optionalTime(repo.UpdateTime),
		},
	}}
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
