package registry

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

var (
	created = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	updated = created.Add(time.Hour)
)

func repositoryHarbor() *fakeHarbor {
	return &fakeHarbor{repositories: []harbor.Repository{
		{ID: 1, Name: "proj/nginx", Description: "web", ArtifactCount: 3, PullCount: 120, CreationTime: created, UpdateTime: updated},
		{ID: 2, Name: "proj/team/api", ArtifactCount: 1, CreationTime: created},
		{ID: 3, Name: "proj/dotted.name", CreationTime: created},
	}}
}

func newRepositories(t *testing.T) (*Repositories, *fixture) {
	t.Helper()
	f := newFixture(t, repositoryHarbor())
	return f.repositories, f
}

func listItems(t *testing.T, r *Repositories, namespace string, opts *metainternalversion.ListOptions) []v1alpha1.HarborRepository {
	t.Helper()
	obj, err := r.List(inNamespace(namespace), opts)
	if err != nil {
		t.Fatal(err)
	}
	return obj.(*v1alpha1.HarborRepositoryList).Items
}

func names(items []v1alpha1.HarborRepository) []string {
	var n []string
	for _, r := range items {
		n = append(n, r.Namespace+"/"+r.Name)
	}
	return n
}

var dottedRepository = repositoryObjectName("dotted.name")

func TestListRepositories(t *testing.T) {
	r, _ := newRepositories(t)
	items := listItems(t, r, "ns1", nil)

	want := v1alpha1.HarborRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "nginx",
			Namespace:         "ns1",
			UID:               "00574b18-6628-5e46-960b-b7011fb281e7",
			CreationTimestamp: metav1.NewTime(created),
		},
		Status: v1alpha1.HarborRepositoryStatus{
			Name:          "proj/nginx",
			Description:   "web",
			ArtifactCount: 3,
			PullCount:     120,
			UpdateTime:    &metav1.Time{Time: updated},
		},
	}
	if diff := cmp.Diff(want, items[1]); diff != "" {
		t.Errorf("nginx (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"ns1/" + dottedRepository, "ns1/nginx", "ns1/team.api"}, names(items)); diff != "" {
		t.Errorf("names (-want +got):\n%s", diff)
	}
	if items[2].Status.UpdateTime != nil {
		t.Errorf("team/api has update time %v, want none", items[2].Status.UpdateTime)
	}
}

func TestListRepositoriesAcrossNamespaces(t *testing.T) {
	r, _ := newRepositories(t)
	want := []string{"ns1/" + dottedRepository, "ns1/nginx", "ns1/team.api", "ns2/" + dottedRepository, "ns2/nginx", "ns2/team.api"}
	if diff := cmp.Diff(want, names(listItems(t, r, "", nil))); diff != "" {
		t.Errorf("names (-want +got):\n%s", diff)
	}
}

func TestRepositoryUIDs(t *testing.T) {
	r, f := newRepositories(t)
	before := listItems(t, r, "", nil)
	seen := map[types.UID]string{}
	for _, repo := range before {
		if other, ok := seen[repo.UID]; ok {
			t.Errorf("%s/%s and %s share UID %s", repo.Namespace, repo.Name, other, repo.UID)
		}
		seen[repo.UID] = repo.Namespace + "/" + repo.Name
	}

	f.harbor.repositories[0].ID = 4
	f.poll(t)
	after := listItems(t, r, "", nil)
	if after[1].UID == before[1].UID {
		t.Error("a recreated repository kept its UID")
	}
	if after[2].UID != before[2].UID {
		t.Error("an unchanged repository changed its UID")
	}
}

func TestListRepositoriesInDisallowedNamespace(t *testing.T) {
	r, _ := newRepositories(t)
	if n := names(listItems(t, r, "other", nil)); len(n) != 0 {
		t.Errorf("got %v", n)
	}
}

func TestListRepositoriesWithSelectors(t *testing.T) {
	r, _ := newRepositories(t)
	for _, tc := range []struct {
		opts *metainternalversion.ListOptions
		want []string
	}{
		{&metainternalversion.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", "team.api")}, []string{"ns1/team.api", "ns2/team.api"}},
		{&metainternalversion.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.namespace", "ns2"), LabelSelector: labels.Everything()}, []string{"ns2/" + dottedRepository, "ns2/nginx", "ns2/team.api"}},
		{&metainternalversion.ListOptions{LabelSelector: labels.SelectorFromSet(labels.Set{"a": "b"})}, nil},
	} {
		if diff := cmp.Diff(tc.want, names(listItems(t, r, "", tc.opts))); diff != "" {
			t.Errorf("%v (-want +got):\n%s", tc.opts, diff)
		}
	}
}

func TestGetRepository(t *testing.T) {
	r, _ := newRepositories(t)
	listed := listItems(t, r, "ns2", nil)
	for _, want := range listed {
		obj, err := r.Get(inNamespace("ns2"), want.Name, &metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(&want, obj); diff != "" {
			t.Errorf("%s: get differs from list (-list +get):\n%s", want.Name, diff)
		}
	}
}

func TestGetRepositoryNotFound(t *testing.T) {
	for _, tc := range []struct{ namespace, name string }{
		{"ns1", "missing"},
		{"ns1", "dotted.name"},
		{"ns1", "Not_A_Name"},
		{"other", "nginx"},
	} {
		r, _ := newRepositories(t)
		_, err := r.Get(inNamespace(tc.namespace), tc.name, &metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			t.Errorf("%s/%s: got %v, want NotFound", tc.namespace, tc.name, err)
		}
	}
}

func TestRequestsDoNotCallHarbor(t *testing.T) {
	r, f := newRepositories(t)
	f.harbor.calls = nil
	listItems(t, r, "", nil)
	if _, err := r.Get(inNamespace("ns1"), "nginx", &metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(f.harbor.calls) > 0 {
		t.Errorf("called Harbor: %v", f.harbor.calls)
	}
}

func TestRepositoryTable(t *testing.T) {
	h := repositoryHarbor()
	h.repositories[0].CreationTime = time.Now().Add(-10 * time.Hour)
	h.repositories[1].CreationTime = time.Time{}
	h.repositories[2].CreationTime = time.Now().Add(-72 * time.Hour)
	r := newFixture(t, h).repositories
	list, err := r.List(inNamespace("ns1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	table, err := r.ConvertToTable(context.Background(), list, nil)
	if err != nil {
		t.Fatal(err)
	}
	var columns []string
	for _, c := range table.ColumnDefinitions {
		columns = append(columns, c.Name)
	}
	if diff := cmp.Diff([]string{"Name", "Repository", "Artifacts", "Pulls", "Age"}, columns); diff != "" {
		t.Errorf("columns (-want +got):\n%s", diff)
	}
	var cells [][]any
	for _, row := range table.Rows {
		cells = append(cells, row.Cells)
	}
	wantCells := [][]any{
		{dottedRepository, "proj/dotted.name", int64(0), int64(0), "3d"},
		{"nginx", "proj/nginx", int64(3), int64(120), "10h"},
		{"team.api", "proj/team/api", int64(1), int64(0), "<unknown>"},
	}
	if diff := cmp.Diff(wantCells, cells); diff != "" {
		t.Errorf("cells (-want +got):\n%s", diff)
	}

	one, err := r.Get(inNamespace("ns1"), "nginx", &metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if table, err := r.ConvertToTable(context.Background(), one, nil); err != nil || len(table.Rows) != 1 {
		t.Errorf("single object: %v, %v", table, err)
	}
}

func TestRepositoryTableOfEmptyList(t *testing.T) {
	r, _ := newRepositories(t)
	listMeta := metav1.ListMeta{ResourceVersion: "7", Continue: "next"}
	table, err := r.ConvertToTable(context.Background(), &v1alpha1.HarborRepositoryList{ListMeta: listMeta}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(listMeta, table.ListMeta); diff != "" {
		t.Errorf("list metadata (-want +got):\n%s", diff)
	}
	b, err := json.Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"rows":[]`) {
		t.Errorf("got %s, want empty rows", b)
	}
}
