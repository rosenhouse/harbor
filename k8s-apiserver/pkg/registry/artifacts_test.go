package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

var (
	pushed = time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	pulled = pushed.Add(time.Hour)
)

// digest returns a sha256 digest starting with prefix.
func digest(prefix string) string {
	return "sha256:" + prefix + strings.Repeat("0", 64-len(prefix))
}

func newArtifacts() (*Artifacts, *fakeHarbor) {
	h := &fakeHarbor{
		repositories: []harbor.Repository{
			{ID: 1, Name: "proj/team/api"},
			{ID: 2, Name: "proj/nginx"},
			{ID: 3, Name: "proj/dotted.name"},
		},
		artifacts: map[string][]harbor.Artifact{
			"proj/team/api": {{
				ID:                30,
				Digest:            digest("aaaaaaaaaaaa"),
				RepositoryName:    "proj/team/api",
				Type:              "IMAGE",
				MediaType:         "application/vnd.oci.image.config.v1+json",
				ManifestMediaType: "application/vnd.oci.image.manifest.v1+json",
				ArtifactType:      "application/vnd.example",
				Size:              1536,
				PushTime:          pushed,
				PullTime:          pulled,
				Annotations:       map[string]string{"org.opencontainers.image.title": "api"},
				Tags: []harbor.Tag{
					{Name: "v1", PushTime: pushed, PullTime: pulled},
					{Name: "latest", PushTime: pushed},
				},
			}},
			"proj/nginx": {
				{ID: 21, Digest: digest("111111111111")},
				{ID: 20, Digest: digest("222222222222")},
				{ID: 22, Digest: "sha256:short"},
			},
			"proj/dotted.name": {{ID: 10, Digest: digest("dddddddddddd")}},
		},
	}
	return NewArtifacts(h, "proj", fakeNamespaces{"ns1", "ns2"}), h
}

var dottedArtifact = repositoryObjectName("dotted.name") + ".sha256-dddddddddddd"

func artifactItems(t *testing.T, a *Artifacts, namespace string, opts *metainternalversion.ListOptions) []v1alpha1.HarborArtifact {
	t.Helper()
	obj, err := a.List(inNamespace(namespace), opts)
	if err != nil {
		t.Fatal(err)
	}
	return obj.(*v1alpha1.HarborArtifactList).Items
}

func artifactNames(items []v1alpha1.HarborArtifact) []string {
	var n []string
	for _, a := range items {
		n = append(n, a.Namespace+"/"+a.Name)
	}
	return n
}

func sortedCalls(h *fakeHarbor) []string {
	return slices.Sorted(slices.Values(h.calls))
}

func TestListArtifacts(t *testing.T) {
	a, _ := newArtifacts()
	items := artifactItems(t, a, "ns1", nil)

	want := []string{"ns1/" + dottedArtifact, "ns1/nginx.sha256-222222222222", "ns1/nginx.sha256-111111111111", "ns1/team.api.sha256-aaaaaaaaaaaa"}
	if diff := cmp.Diff(want, artifactNames(items)); diff != "" {
		t.Errorf("names (-want +got):\n%s", diff)
	}

	wantAPI := v1alpha1.HarborArtifact{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "team.api.sha256-aaaaaaaaaaaa",
			Namespace:         "ns1",
			UID:               "7167cbdd-03b7-5234-aca1-8f6755c64a8a",
			CreationTimestamp: metav1.NewTime(pushed),
			Labels:            map[string]string{v1alpha1.RepositoryLabel: "team.api"},
		},
		Status: v1alpha1.HarborArtifactStatus{
			Repository:      "proj/team/api",
			Digest:          digest("aaaaaaaaaaaa"),
			Type:            "IMAGE",
			MediaType:       "application/vnd.oci.image.manifest.v1+json",
			ConfigMediaType: "application/vnd.oci.image.config.v1+json",
			ArtifactType:    "application/vnd.example",
			Size:            1536,
			Tags: []v1alpha1.HarborTag{
				{Name: "latest", PushTime: &metav1.Time{Time: pushed}},
				{Name: "v1", PushTime: &metav1.Time{Time: pushed}, PullTime: &metav1.Time{Time: pulled}},
			},
			PushTime:    &metav1.Time{Time: pushed},
			PullTime:    &metav1.Time{Time: pulled},
			Annotations: map[string]string{"org.opencontainers.image.title": "api"},
		},
	}
	if diff := cmp.Diff(wantAPI, items[3]); diff != "" {
		t.Errorf("team/api artifact (-want +got):\n%s", diff)
	}

	nginx := items[1]
	if nginx.Status.PushTime != nil || nginx.Status.PullTime != nil || nginx.Status.Repository != "proj/nginx" {
		t.Errorf("nginx artifact: push time %v, pull time %v, repository %q", nginx.Status.PushTime, nginx.Status.PullTime, nginx.Status.Repository)
	}
	if items[1].UID == items[2].UID {
		t.Error("artifacts share a UID")
	}
}

func TestIndexArtifacts(t *testing.T) {
	for _, mediaType := range []string{"application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json"} {
		h := &fakeHarbor{artifacts: map[string][]harbor.Artifact{"proj/multi": {{
			ID:                1,
			Digest:            digest("aaaaaaaaaaaa"),
			MediaType:         mediaType,
			ManifestMediaType: mediaType,
			References: []harbor.Reference{
				{ChildDigest: digest("bbbbbbbbbbbb"), Platform: &harbor.Platform{Architecture: "arm64", OS: "linux", Variant: "v8"}},
				{ChildDigest: digest("cccccccccccc")},
			},
		}}}}
		obj, err := NewArtifacts(h, "proj", fakeNamespaces{"ns1"}).Get(inNamespace("ns1"), "multi.sha256-aaaaaaaaaaaa", &metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		want := v1alpha1.HarborArtifactStatus{
			Repository: "proj/multi",
			Digest:     digest("aaaaaaaaaaaa"),
			MediaType:  mediaType,
			References: []v1alpha1.HarborArtifactReference{
				{Digest: digest("bbbbbbbbbbbb"), Platform: &v1alpha1.HarborPlatform{Architecture: "arm64", OS: "linux", Variant: "v8"}},
				{Digest: digest("cccccccccccc")},
			},
		}
		if diff := cmp.Diff(want, obj.(*v1alpha1.HarborArtifact).Status); diff != "" {
			t.Errorf("%s: status (-want +got):\n%s", mediaType, diff)
		}
	}
}

func TestListArtifactsAcrossNamespaces(t *testing.T) {
	a, _ := newArtifacts()
	items := artifactItems(t, a, "", nil)
	var want []string
	for _, ns := range []string{"ns1", "ns2"} {
		for _, name := range []string{dottedArtifact, "nginx.sha256-222222222222", "nginx.sha256-111111111111", "team.api.sha256-aaaaaaaaaaaa"} {
			want = append(want, ns+"/"+name)
		}
	}
	if diff := cmp.Diff(want, artifactNames(items)); diff != "" {
		t.Errorf("names (-want +got):\n%s", diff)
	}
	if items[0].UID == items[4].UID {
		t.Error("copies in different namespaces share a UID")
	}
}

func TestArtifactUIDsDifferFromRepositoryUIDs(t *testing.T) {
	h := &fakeHarbor{
		repositories: []harbor.Repository{{ID: 1, Name: "proj/app"}},
		artifacts:    map[string][]harbor.Artifact{"proj/app": {{ID: 1, Digest: digest("aaaaaaaaaaaa")}}},
	}
	repo, err := NewRepositories(h, "proj", fakeNamespaces{"ns1"}).Get(inNamespace("ns1"), "app", &metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := NewArtifacts(h, "proj", fakeNamespaces{"ns1"}).Get(inNamespace("ns1"), "app.sha256-aaaaaaaaaaaa", &metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if uid := repo.(*v1alpha1.HarborRepository).UID; uid == artifact.(*v1alpha1.HarborArtifact).UID {
		t.Errorf("repository and artifact share UID %s", uid)
	}
}

func TestListArtifactsInDisallowedNamespace(t *testing.T) {
	a, h := newArtifacts()
	if n := artifactNames(artifactItems(t, a, "other", nil)); len(n) != 0 || len(h.calls) != 0 {
		t.Errorf("got %v after Harbor calls %v", n, h.calls)
	}
}

func TestListArtifactsWithSelectors(t *testing.T) {
	everything := []string{"artifacts proj dotted.name", "artifacts proj nginx", "artifacts proj team/api", "list proj"}
	byLabel := func(value string) *metainternalversion.ListOptions {
		return &metainternalversion.ListOptions{LabelSelector: labels.SelectorFromSet(labels.Set{v1alpha1.RepositoryLabel: value})}
	}
	byFields := func(set fields.Set) *metainternalversion.ListOptions {
		return &metainternalversion.ListOptions{FieldSelector: fields.SelectorFromSet(set)}
	}
	byRepository := func(value string) *metainternalversion.ListOptions {
		return byFields(fields.Set{"status.repository": value})
	}
	byName := func(value string) *metainternalversion.ListOptions {
		return byFields(fields.Set{"metadata.name": value})
	}
	notTeamAPI, err := labels.Parse(v1alpha1.RepositoryLabel + "!=team.api")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		desc      string
		opts      *metainternalversion.ListOptions
		want      []string
		wantCalls []string
	}{
		{"label", byLabel("team.api"), []string{"team.api.sha256-aaaaaaaaaaaa"}, []string{"artifacts proj team/api"}},
		{"hashed label", byLabel(repositoryObjectName("dotted.name")), []string{dottedArtifact}, []string{"artifacts proj dotted.name", "list proj"}},
		{"missing label", byLabel("missing"), nil, []string{"artifacts proj missing"}},
		{"missing hashed label", byLabel("missing-0123456789"), nil, []string{"list proj"}},
		{"label that is not a name", byLabel("Not_A_Name"), nil, nil},
		{"label inequality", &metainternalversion.ListOptions{LabelSelector: notTeamAPI}, []string{dottedArtifact, "nginx.sha256-222222222222", "nginx.sha256-111111111111"}, everything},
		{"repository", byRepository("proj/team/api"), []string{"team.api.sha256-aaaaaaaaaaaa"}, []string{"artifacts proj team/api"}},
		{"dotted repository", byRepository("proj/dotted.name"), []string{dottedArtifact}, []string{"artifacts proj dotted.name"}},
		{"repository and label", &metainternalversion.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("status.repository", "proj/nginx"),
			LabelSelector: labels.SelectorFromSet(labels.Set{v1alpha1.RepositoryLabel: "team.api"}),
		}, nil, []string{"artifacts proj nginx"}},
		{"missing repository", byRepository("proj/missing"), nil, []string{"artifacts proj missing"}},
		{"repository in another project", byRepository("other/team/api"), nil, nil},
		{"project", byRepository("proj/"), nil, nil},
		{"repository with a dot segment", byRepository("proj/."), nil, nil},
		{"repository with a dot-dot segment", byRepository("proj/team/.."), nil, nil},
		{"repository with an empty segment", byRepository("proj/team//api"), nil, nil},
		{"repository with a trailing slash", byRepository("proj/team/api/"), nil, nil},
		{"repository with capitals", byRepository("proj/Team/api"), nil, nil},
		{"name", byName("nginx.sha256-111111111111"), []string{"nginx.sha256-111111111111"}, []string{"artifacts proj nginx sha256:111111111111"}},
		{"hashed name", byName(dottedArtifact), []string{dottedArtifact}, []string{"artifacts proj dotted.name sha256:dddddddddddd", "list proj"}},
		{"missing name", byName("nginx.sha256-ffffffffffff"), nil, []string{"artifacts proj nginx sha256:ffffffffffff"}},
		{"name of a repository", byName("nginx"), nil, nil},
		{"name in another repository", byFields(fields.Set{"metadata.name": "nginx.sha256-111111111111", "status.repository": "proj/team/api"}),
			nil, []string{"artifacts proj nginx sha256:111111111111"}},
		{"unrelated label", &metainternalversion.ListOptions{LabelSelector: labels.SelectorFromSet(labels.Set{"a": "b"})}, nil, everything},
	} {
		a, h := newArtifacts()
		var want []string
		for _, n := range tc.want {
			want = append(want, "ns1/"+n)
		}
		if diff := cmp.Diff(want, artifactNames(artifactItems(t, a, "ns1", tc.opts))); diff != "" {
			t.Errorf("%s: names (-want +got):\n%s", tc.desc, diff)
		}
		if diff := cmp.Diff(tc.wantCalls, sortedCalls(h)); diff != "" {
			t.Errorf("%s: Harbor calls (-want +got):\n%s", tc.desc, diff)
		}
	}
}

func TestListArtifactsSkipsDeletedRepository(t *testing.T) {
	a, h := newArtifacts()
	h.repositories = append(h.repositories, harbor.Repository{ID: 4, Name: "proj/deleted"})
	if n := artifactNames(artifactItems(t, a, "ns1", nil)); len(n) != 4 {
		t.Errorf("got %v", n)
	}
}

func TestListArtifactsConcurrently(t *testing.T) {
	h := &fakeHarbor{artifacts: map[string][]harbor.Artifact{}, delay: 20 * time.Millisecond}
	var want []string
	for i := range 10 {
		repository := fmt.Sprintf("r%d", i)
		h.repositories = append(h.repositories, harbor.Repository{ID: int64(i), Name: "proj/" + repository})
		h.artifacts["proj/"+repository] = []harbor.Artifact{{ID: int64(i), Digest: digest("aaaaaaaaaaaa")}}
		want = append(want, "ns1/"+repository+".sha256-aaaaaaaaaaaa")
	}
	items := artifactItems(t, NewArtifacts(h, "proj", fakeNamespaces{"ns1"}), "ns1", nil)
	if diff := cmp.Diff(want, artifactNames(items)); diff != "" {
		t.Errorf("names (-want +got):\n%s", diff)
	}
	if h.maxInFlight != 4 {
		t.Errorf("listed %d repositories at once, want 4", h.maxInFlight)
	}
}

func TestArtifactsWithSameName(t *testing.T) {
	older, newer := digest("0123456789ab1"), digest("0123456789ab2")
	h := &fakeHarbor{
		repositories: []harbor.Repository{{ID: 1, Name: "proj/app"}},
		artifacts:    map[string][]harbor.Artifact{"proj/app": {{ID: 2, Digest: newer}, {ID: 1, Digest: older}}},
	}
	a := NewArtifacts(h, "proj", fakeNamespaces{"ns1"})
	items := artifactItems(t, a, "ns1", nil)
	if len(items) != 1 || items[0].Status.Digest != older {
		t.Errorf("listed %v, want only %s", items, older)
	}
	obj, err := a.Get(inNamespace("ns1"), "app.sha256-0123456789ab", &metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := obj.(*v1alpha1.HarborArtifact).Status.Digest; got != older {
		t.Errorf("got %s, want %s", got, older)
	}
}

func TestArtifactLabels(t *testing.T) {
	for _, tc := range []struct {
		repository string
		want       map[string]string
	}{
		{strings.Repeat("a", 63), map[string]string{v1alpha1.RepositoryLabel: strings.Repeat("a", 63)}},
		{strings.Repeat("a", 64), nil},
	} {
		h := &fakeHarbor{
			repositories: []harbor.Repository{{ID: 1, Name: "proj/" + tc.repository}},
			artifacts:    map[string][]harbor.Artifact{"proj/" + tc.repository: {{ID: 1, Digest: digest("aaaaaaaaaaaa")}}},
		}
		items := artifactItems(t, NewArtifacts(h, "proj", fakeNamespaces{"ns1"}), "ns1", nil)
		if diff := cmp.Diff(tc.want, items[0].Labels); diff != "" {
			t.Errorf("%d-character repository: labels (-want +got):\n%s", len(tc.repository), diff)
		}
	}
}

func TestGetArtifact(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wantCalls []string
	}{
		{"team.api.sha256-aaaaaaaaaaaa", []string{"artifacts proj team/api sha256:aaaaaaaaaaaa"}},
		{"nginx.sha256-111111111111", []string{"artifacts proj nginx sha256:111111111111"}},
		{dottedArtifact, []string{"list proj", "artifacts proj dotted.name sha256:dddddddddddd"}},
	} {
		a, h := newArtifacts()
		obj, err := a.Get(inNamespace("ns2"), tc.name, &metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(tc.wantCalls, h.calls); diff != "" {
			t.Errorf("%s: Harbor calls (-want +got):\n%s", tc.name, diff)
		}

		items := artifactItems(t, a, "ns2", nil)
		listed := items[slices.IndexFunc(items, func(x v1alpha1.HarborArtifact) bool { return x.Name == tc.name })]
		if diff := cmp.Diff(listed, *obj.(*v1alpha1.HarborArtifact)); diff != "" {
			t.Errorf("%s: get differs from list (-list +get):\n%s", tc.name, diff)
		}
	}
}

func TestGetArtifactInLongRepository(t *testing.T) {
	repository := strings.Repeat("a", 240)
	h := &fakeHarbor{
		repositories: []harbor.Repository{{ID: 1, Name: "proj/" + repository}},
		artifacts:    map[string][]harbor.Artifact{"proj/" + repository: {{ID: 1, Digest: digest("aaaaaaaaaaaa")}}},
	}
	a := NewArtifacts(h, "proj", fakeNamespaces{"ns1"})
	name := artifactItems(t, a, "ns1", nil)[0].Name
	if _, err := a.Get(inNamespace("ns1"), name, &metav1.GetOptions{}); err != nil {
		t.Errorf("get %s: %v", name, err)
	}
}

func TestGetArtifactNotFound(t *testing.T) {
	for _, tc := range []struct {
		namespace, name string
		wantCalls       []string
	}{
		{"ns1", "team.api", nil},
		{"ns1", "Team.api.sha256-aaaaaaaaaaaa", nil},
		{"ns1", strings.Repeat("a", 240) + ".sha256-aaaaaaaaaaaa", nil},
		{"ns1", "team.api.sha256-ffffffffffff", []string{"artifacts proj team/api sha256:ffffffffffff"}},
		{"ns1", "team.api.sha512-aaaaaaaaaaaa", []string{"artifacts proj team/api sha512:aaaaaaaaaaaa"}},
		{"ns1", "nginx.sha256-aaaaaaaaaaaa", []string{"artifacts proj nginx sha256:aaaaaaaaaaaa"}},
		{"ns1", "missing.sha256-aaaaaaaaaaaa", []string{"artifacts proj missing sha256:aaaaaaaaaaaa"}},
		{"ns1", "missing-0123456789.sha256-aaaaaaaaaaaa", []string{"list proj"}},
		{"other", "team.api.sha256-aaaaaaaaaaaa", nil},
	} {
		a, h := newArtifacts()
		_, err := a.Get(inNamespace(tc.namespace), tc.name, &metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			t.Errorf("%s/%s: got %v, want NotFound", tc.namespace, tc.name, err)
		}
		if diff := cmp.Diff(tc.wantCalls, h.calls); diff != "" {
			t.Errorf("%s/%s: Harbor calls (-want +got):\n%s", tc.namespace, tc.name, diff)
		}
	}
}

func TestArtifactHarborErrors(t *testing.T) {
	for _, tc := range []struct {
		harborErr error
		want      func(error) bool
	}{
		{fmt.Errorf("%w: dial tcp 10.0.0.1:443: refused", harbor.ErrUnavailable), apierrors.IsServiceUnavailable},
		{harbor.ErrUnauthorized, apierrors.IsInternalError},
		{harbor.ErrForbidden, apierrors.IsInternalError},
		{errors.New("boom"), apierrors.IsInternalError},
	} {
		a, h := newArtifacts()
		h.err = tc.harborErr
		for _, name := range []string{"team.api.sha256-aaaaaaaaaaaa", dottedArtifact} {
			if _, err := a.Get(inNamespace("ns1"), name, &metav1.GetOptions{}); !tc.want(err) {
				t.Errorf("get %s with %v: got %v", name, tc.harborErr, err)
			}
		}
		_, err := a.List(inNamespace("ns1"), nil)
		if !tc.want(err) {
			t.Errorf("list with %v: got %v", tc.harborErr, err)
		}
		if strings.Contains(err.Error(), "10.0.0.1") || strings.Contains(err.Error(), "boom") {
			t.Errorf("list with %v: error %q leaks details", tc.harborErr, err)
		}
	}
}

// failingArtifacts fails to list nginx's artifacts, and lists the others only once the request is cancelled.
type failingArtifacts struct {
	*fakeHarbor
	cancelled chan string
}

func (f failingArtifacts) ListArtifacts(ctx context.Context, project, repository, digestPrefix string) ([]harbor.Artifact, error) {
	if repository == "nginx" {
		return nil, harbor.ErrUnavailable
	}
	select {
	case <-ctx.Done():
		f.cancelled <- repository
	case <-time.After(5 * time.Second):
	}
	return f.fakeHarbor.ListArtifacts(ctx, project, repository, digestPrefix)
}

func TestListArtifactsStopsAtFirstFailure(t *testing.T) {
	_, h := newArtifacts()
	f := failingArtifacts{h, make(chan string, len(h.repositories))}
	_, err := NewArtifacts(f, "proj", fakeNamespaces{"ns1"}).List(inNamespace("ns1"), nil)
	if !apierrors.IsServiceUnavailable(err) {
		t.Errorf("got %v, want ServiceUnavailable", err)
	}
	close(f.cancelled)
	var cancelled []string
	for r := range f.cancelled {
		cancelled = append(cancelled, r)
	}
	if diff := cmp.Diff([]string{"dotted.name", "team/api"}, slices.Sorted(slices.Values(cancelled))); diff != "" {
		t.Errorf("cancelled lists (-want +got):\n%s", diff)
	}
}

func TestArtifactTable(t *testing.T) {
	a, _ := newArtifacts()
	list, err := a.List(inNamespace("ns1"), &metainternalversion.ListOptions{FieldSelector: fields.OneTermEqualSelector("status.repository", "proj/team/api")})
	if err != nil {
		t.Fatal(err)
	}
	table, err := a.ConvertToTable(context.Background(), list, nil)
	if err != nil {
		t.Fatal(err)
	}
	var columns []string
	for _, c := range table.ColumnDefinitions {
		columns = append(columns, c.Name)
	}
	if diff := cmp.Diff([]string{"Name", "Repository", "Tags", "Type", "Size", "Age"}, columns); diff != "" {
		t.Errorf("columns (-want +got):\n%s", diff)
	}
	if len(table.Rows) != 1 {
		t.Fatalf("got %d rows", len(table.Rows))
	}
	if diff := cmp.Diff([]any{"team.api.sha256-aaaaaaaaaaaa", "proj/team/api", "latest,v1", "IMAGE", "1.5KiB"}, table.Rows[0].Cells[:5]); diff != "" {
		t.Errorf("row (-want +got):\n%s", diff)
	}

	untagged, err := a.Get(inNamespace("ns1"), "nginx.sha256-111111111111", &metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	table, err = a.ConvertToTable(context.Background(), untagged, nil)
	if err != nil || len(table.Rows) != 1 {
		t.Fatalf("single object: %v, %v", table, err)
	}
	if tags := table.Rows[0].Cells[2]; tags != "<none>" {
		t.Errorf("untagged artifact has tags %q", tags)
	}

	if _, err := a.ConvertToTable(context.Background(), &v1alpha1.HarborRepository{}, nil); err == nil {
		t.Error("converted a HarborRepository")
	}
}

func TestArtifactTableOfEmptyList(t *testing.T) {
	a, _ := newArtifacts()
	listMeta := metav1.ListMeta{ResourceVersion: "7", Continue: "next"}
	table, err := a.ConvertToTable(context.Background(), &v1alpha1.HarborArtifactList{ListMeta: listMeta}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(listMeta, table.ListMeta); diff != "" {
		t.Errorf("list metadata (-want +got):\n%s", diff)
	}
	if b, _ := json.Marshal(table); !strings.Contains(string(b), `"rows":[]`) {
		t.Errorf("got %s, want empty rows", b)
	}
}

func TestHumanSize(t *testing.T) {
	for size, want := range map[int64]string{
		0:           "0B",
		1023:        "1023B",
		1024:        "1.0KiB",
		1536:        "1.5KiB",
		3 << 19:     "1.5MiB",
		5 << 30:     "5.0GiB",
		1 << 62:     "4.0EiB",
		1<<63 - 1:   "8.0EiB",
		1<<40 - 1:   "1.0TiB",
		1<<40 + 1:   "1.0TiB",
		7 << 50 / 2: "3.5PiB",
	} {
		if got := humanSize(size); got != want {
			t.Errorf("%d: got %q, want %q", size, got, want)
		}
	}
}
