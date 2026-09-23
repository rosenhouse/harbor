package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

var (
	created = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	updated = created.Add(time.Hour)
)

func newRepositories() (*Repositories, *fakeHarbor) {
	h := &fakeHarbor{repositories: []harbor.Repository{
		{ID: 1, Name: "proj/nginx", Description: "web", ArtifactCount: 3, PullCount: 120, CreationTime: created, UpdateTime: updated},
		{ID: 2, Name: "proj/team/api", ArtifactCount: 1, CreationTime: created},
		{ID: 3, Name: "proj/dotted.name", CreationTime: created},
	}}
	return NewRepositories(h, "proj", fakeNamespaces{"ns1", "ns2"}), h
}

func inNamespace(ns string) context.Context {
	return genericapirequest.WithNamespace(context.Background(), ns)
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

func TestListRepositories(t *testing.T) {
	r, _ := newRepositories()
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
	if diff := cmp.Diff(want, items[0]); diff != "" {
		t.Errorf("nginx (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"ns1/nginx", "ns1/team.api", "ns1/" + repositoryObjectName("dotted.name")}, names(items)); diff != "" {
		t.Errorf("names (-want +got):\n%s", diff)
	}
	if items[1].Status.UpdateTime != nil {
		t.Errorf("team/api has update time %v, want none", items[1].Status.UpdateTime)
	}
}

func TestListRepositoriesAcrossNamespaces(t *testing.T) {
	r, _ := newRepositories()
	want := []string{"ns1/nginx", "ns1/team.api", "ns1/" + repositoryObjectName("dotted.name"), "ns2/nginx", "ns2/team.api", "ns2/" + repositoryObjectName("dotted.name")}
	if diff := cmp.Diff(want, names(listItems(t, r, "", nil))); diff != "" {
		t.Errorf("names (-want +got):\n%s", diff)
	}
}

func TestRepositoryUIDs(t *testing.T) {
	r, h := newRepositories()
	before := listItems(t, r, "", nil)
	seen := map[types.UID]string{}
	for _, repo := range before {
		if other, ok := seen[repo.UID]; ok {
			t.Errorf("%s/%s and %s share UID %s", repo.Namespace, repo.Name, other, repo.UID)
		}
		seen[repo.UID] = repo.Namespace + "/" + repo.Name
	}

	h.repositories[0].ID = 4
	after := listItems(t, r, "", nil)
	if after[0].UID == before[0].UID {
		t.Error("a recreated repository kept its UID")
	}
	if after[1].UID != before[1].UID {
		t.Error("an unchanged repository changed its UID")
	}
}

func TestListRepositoriesInDisallowedNamespace(t *testing.T) {
	r, h := newRepositories()
	if n := names(listItems(t, r, "other", nil)); len(n) != 0 || len(h.calls) != 0 {
		t.Errorf("got %v after Harbor calls %v", n, h.calls)
	}
}

func TestListRepositoriesWithSelectors(t *testing.T) {
	r, _ := newRepositories()
	for _, tc := range []struct {
		opts *metainternalversion.ListOptions
		want []string
	}{
		{&metainternalversion.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", "team.api")}, []string{"ns1/team.api", "ns2/team.api"}},
		{&metainternalversion.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.namespace", "ns2"), LabelSelector: labels.Everything()}, []string{"ns2/nginx", "ns2/team.api", "ns2/" + repositoryObjectName("dotted.name")}},
		{&metainternalversion.ListOptions{LabelSelector: labels.SelectorFromSet(labels.Set{"a": "b"})}, nil},
	} {
		if diff := cmp.Diff(tc.want, names(listItems(t, r, "", tc.opts))); diff != "" {
			t.Errorf("%v (-want +got):\n%s", tc.opts, diff)
		}
	}
}

func TestGetRepository(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wantCalls []string
	}{
		{"team.api", []string{"get proj team/api"}},
		{repositoryObjectName("dotted.name"), []string{"list proj"}},
	} {
		r, h := newRepositories()
		obj, err := r.Get(inNamespace("ns2"), tc.name, &metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		got := obj.(*v1alpha1.HarborRepository)
		if got.Name != tc.name || got.Namespace != "ns2" {
			t.Errorf("got %s/%s", got.Namespace, got.Name)
		}
		if diff := cmp.Diff(tc.wantCalls, h.calls); diff != "" {
			t.Errorf("%s: Harbor calls (-want +got):\n%s", tc.name, diff)
		}
	}
}

func TestGetRepositoryNotFound(t *testing.T) {
	for _, tc := range []struct {
		namespace, name string
		wantCalls       []string
	}{
		{"ns1", "missing", []string{"get proj missing"}},
		{"ns1", "dotted.name", []string{"get proj dotted/name"}},
		{"ns1", "missing-0123456789", []string{"list proj"}},
		{"ns1", "Not_A_Name", nil},
		{"other", "nginx", nil},
	} {
		r, h := newRepositories()
		_, err := r.Get(inNamespace(tc.namespace), tc.name, &metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			t.Errorf("%s/%s: got %v, want NotFound", tc.namespace, tc.name, err)
		}
		if diff := cmp.Diff(tc.wantCalls, h.calls); diff != "" {
			t.Errorf("%s/%s: Harbor calls (-want +got):\n%s", tc.namespace, tc.name, diff)
		}
	}
}

func TestRepositoryHarborErrors(t *testing.T) {
	for _, tc := range []struct {
		harborErr error
		want      func(error) bool
	}{
		{fmt.Errorf("%w: dial tcp 10.0.0.1:443: refused", harbor.ErrUnavailable), apierrors.IsServiceUnavailable},
		{harbor.ErrUnauthorized, apierrors.IsInternalError},
		{harbor.ErrForbidden, apierrors.IsInternalError},
		{errors.New("boom"), apierrors.IsInternalError},
	} {
		r, h := newRepositories()
		h.err = tc.harborErr
		if _, err := r.Get(inNamespace("ns1"), "nginx", &metav1.GetOptions{}); !tc.want(err) {
			t.Errorf("get with %v: got %v", tc.harborErr, err)
		}
		_, err := r.List(inNamespace("ns1"), nil)
		if !tc.want(err) {
			t.Errorf("list with %v: got %v", tc.harborErr, err)
		}
		if strings.Contains(err.Error(), "10.0.0.1") || strings.Contains(err.Error(), "boom") {
			t.Errorf("list with %v: error %q leaks details", tc.harborErr, err)
		}
	}
}

func TestRepositoryTable(t *testing.T) {
	r, h := newRepositories()
	h.repositories[0].CreationTime = time.Now().Add(-10 * time.Hour)
	h.repositories[1].CreationTime = time.Time{}
	h.repositories[2].CreationTime = time.Now().Add(-72 * time.Hour)
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
		{"nginx", "proj/nginx", int64(3), int64(120), "10h"},
		{"team.api", "proj/team/api", int64(1), int64(0), "<unknown>"},
		{repositoryObjectName("dotted.name"), "proj/dotted.name", int64(0), int64(0), "3d"},
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
	r, _ := newRepositories()
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
