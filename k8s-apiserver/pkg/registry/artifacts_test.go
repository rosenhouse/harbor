package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	testingclock "k8s.io/utils/clock/testing"

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

func artifactHarbor() *fakeHarbor {
	return &fakeHarbor{
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
}

func newArtifacts(t *testing.T) (*Artifacts, *fixture) {
	t.Helper()
	f := newFixture(t, artifactHarbor())
	return f.artifacts, f
}

// artifactsOf serves the artifacts of h.
func artifactsOf(t *testing.T, h *fakeHarbor) *Artifacts {
	t.Helper()
	return newFixture(t, h).artifacts
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

func TestListArtifacts(t *testing.T) {
	a, _ := newArtifacts(t)
	items := artifactItems(t, a, "ns1", nil)

	want := []string{"ns1/" + dottedArtifact, "ns1/nginx.sha256-111111111111", "ns1/nginx.sha256-222222222222", "ns1/team.api.sha256-aaaaaaaaaaaa"}
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
		a := artifactsOf(t, &fakeHarbor{
			repositories: []harbor.Repository{{ID: 1, Name: "proj/multi"}},
			artifacts: map[string][]harbor.Artifact{"proj/multi": {{
				ID:                1,
				Digest:            digest("aaaaaaaaaaaa"),
				MediaType:         mediaType,
				ManifestMediaType: mediaType,
				References: []harbor.Reference{
					{ChildDigest: digest("bbbbbbbbbbbb"), Platform: &harbor.Platform{Architecture: "arm64", OS: "linux", Variant: "v8"}},
					{ChildDigest: digest("cccccccccccc")},
				},
			}}},
		})
		obj, err := a.Get(inNamespace("ns1"), "multi.sha256-aaaaaaaaaaaa", &metav1.GetOptions{})
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
	a, _ := newArtifacts(t)
	items := artifactItems(t, a, "", nil)
	var want []string
	for _, ns := range []string{"ns1", "ns2"} {
		for _, name := range []string{dottedArtifact, "nginx.sha256-111111111111", "nginx.sha256-222222222222", "team.api.sha256-aaaaaaaaaaaa"} {
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
	f := newFixture(t, &fakeHarbor{
		repositories: []harbor.Repository{{ID: 1, Name: "proj/app"}},
		artifacts:    map[string][]harbor.Artifact{"proj/app": {{ID: 1, Digest: digest("aaaaaaaaaaaa")}}},
	})
	repo, err := f.repositories.Get(inNamespace("ns1"), "app", &metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := f.artifacts.Get(inNamespace("ns1"), "app.sha256-aaaaaaaaaaaa", &metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if uid := repo.(*v1alpha1.HarborRepository).UID; uid == artifact.(*v1alpha1.HarborArtifact).UID {
		t.Errorf("repository and artifact share UID %s", uid)
	}
}

func TestListArtifactsInDisallowedNamespace(t *testing.T) {
	a, _ := newArtifacts(t)
	if n := artifactNames(artifactItems(t, a, "other", nil)); len(n) != 0 {
		t.Errorf("got %v", n)
	}
}

func byLabel(value string) *metainternalversion.ListOptions {
	return &metainternalversion.ListOptions{LabelSelector: labels.SelectorFromSet(labels.Set{v1alpha1.RepositoryLabel: value})}
}

func byFields(set fields.Set) *metainternalversion.ListOptions {
	return &metainternalversion.ListOptions{FieldSelector: fields.SelectorFromSet(set)}
}

func byRepository(value string) *metainternalversion.ListOptions {
	return byFields(fields.Set{"status.repository": value})
}

func byName(value string) *metainternalversion.ListOptions {
	return byFields(fields.Set{"metadata.name": value})
}

// byLabelSelector lists with a label selector such as "a!=b".
func byLabelSelector(selector string) *metainternalversion.ListOptions {
	s, err := labels.Parse(selector)
	if err != nil {
		panic(err)
	}
	return &metainternalversion.ListOptions{LabelSelector: s}
}

var notTeamAPI = byLabelSelector(v1alpha1.RepositoryLabel + "!=team.api")

func TestListArtifactsWithSelectors(t *testing.T) {
	a, _ := newArtifacts(t)
	for _, tc := range []struct {
		desc string
		opts *metainternalversion.ListOptions
		want []string
	}{
		{"label", byLabel("team.api"), []string{"team.api.sha256-aaaaaaaaaaaa"}},
		{"hashed label", byLabel(repositoryObjectName("dotted.name")), []string{dottedArtifact}},
		{"missing label", byLabel("missing"), nil},
		{"label inequality", notTeamAPI, []string{dottedArtifact, "nginx.sha256-111111111111", "nginx.sha256-222222222222"}},
		{"repository", byRepository("proj/team/api"), []string{"team.api.sha256-aaaaaaaaaaaa"}},
		{"dotted repository", byRepository("proj/dotted.name"), []string{dottedArtifact}},
		{"repository and label", &metainternalversion.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("status.repository", "proj/nginx"),
			LabelSelector: labels.SelectorFromSet(labels.Set{v1alpha1.RepositoryLabel: "team.api"}),
		}, nil},
		{"missing repository", byRepository("proj/missing"), nil},
		{"repository in another project", byRepository("other/team/api"), nil},
		{"name", byName("nginx.sha256-111111111111"), []string{"nginx.sha256-111111111111"}},
		{"hashed name", byName(dottedArtifact), []string{dottedArtifact}},
		{"missing name", byName("nginx.sha256-ffffffffffff"), nil},
		{"name of a repository", byName("nginx"), nil},
		{"name in another repository", byFields(fields.Set{"metadata.name": "nginx.sha256-111111111111", "status.repository": "proj/team/api"}), nil},
		{"unrelated label", &metainternalversion.ListOptions{LabelSelector: labels.SelectorFromSet(labels.Set{"a": "b"})}, nil},
	} {
		var want []string
		for _, n := range tc.want {
			want = append(want, "ns1/"+n)
		}
		if diff := cmp.Diff(want, artifactNames(artifactItems(t, a, "ns1", tc.opts))); diff != "" {
			t.Errorf("%s: names (-want +got):\n%s", tc.desc, diff)
		}
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
		a := artifactsOf(t, &fakeHarbor{
			repositories: []harbor.Repository{{ID: 1, Name: "proj/" + tc.repository}},
			artifacts:    map[string][]harbor.Artifact{"proj/" + tc.repository: {{ID: 1, Digest: digest("aaaaaaaaaaaa")}}},
		})
		items := artifactItems(t, a, "ns1", nil)
		if diff := cmp.Diff(tc.want, items[0].Labels); diff != "" {
			t.Errorf("%d-character repository: labels (-want +got):\n%s", len(tc.repository), diff)
		}
	}
}

func TestGetArtifact(t *testing.T) {
	a, _ := newArtifacts(t)
	for _, want := range artifactItems(t, a, "ns2", nil) {
		obj, err := a.Get(inNamespace("ns2"), want.Name, &metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(&want, obj); diff != "" {
			t.Errorf("%s: get differs from list (-list +get):\n%s", want.Name, diff)
		}
	}
}

func TestGetArtifactInLongRepository(t *testing.T) {
	repository := strings.Repeat("a", 240)
	a := artifactsOf(t, &fakeHarbor{
		repositories: []harbor.Repository{{ID: 1, Name: "proj/" + repository}},
		artifacts:    map[string][]harbor.Artifact{"proj/" + repository: {{ID: 1, Digest: digest("aaaaaaaaaaaa")}}},
	})
	name := artifactItems(t, a, "ns1", nil)[0].Name
	if _, err := a.Get(inNamespace("ns1"), name, &metav1.GetOptions{}); err != nil {
		t.Errorf("get %s: %v", name, err)
	}
}

func TestGetArtifactNotFound(t *testing.T) {
	a, _ := newArtifacts(t)
	for _, tc := range []struct{ namespace, name string }{
		{"ns1", "team.api"},
		{"ns1", "Team.api.sha256-aaaaaaaaaaaa"},
		{"ns1", "team.api.sha256-ffffffffffff"},
		{"ns1", "team.api.sha512-aaaaaaaaaaaa"},
		{"ns1", "nginx.sha256-aaaaaaaaaaaa"},
		{"ns1", "missing.sha256-aaaaaaaaaaaa"},
		{"other", "team.api.sha256-aaaaaaaaaaaa"},
	} {
		_, err := a.Get(inNamespace(tc.namespace), tc.name, &metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			t.Errorf("%s/%s: got %v, want NotFound", tc.namespace, tc.name, err)
		}
	}
}

func TestArtifactTable(t *testing.T) {
	a, _ := newArtifacts(t)
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
	a, _ := newArtifacts(t)
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

// replicatedArtifact is an artifact that replication ns1/nginx copied.
const replicatedArtifact = "k8s.ns1.nginx.library.nginx.sha256-aaaaaaaaaaaa"

// replicatedHarbor holds an artifact that replication ns1/nginx copied, and others.
func replicatedHarbor() *fakeHarbor {
	return &fakeHarbor{
		repositories: []harbor.Repository{
			{ID: 1, Name: "proj/k8s/ns1/nginx/library/nginx"},
			{ID: 2, Name: "proj/k8s/ns1/nginx-old/library/nginx"},
			{ID: 3, Name: "proj/nginx"},
		},
		artifacts: map[string][]harbor.Artifact{
			"proj/k8s/ns1/nginx/library/nginx":     {{ID: 1, Digest: digest("aaaaaaaaaaaa")}},
			"proj/k8s/ns1/nginx-old/library/nginx": {{ID: 2, Digest: digest("bbbbbbbbbbbb")}},
			"proj/nginx":                           {{ID: 3, Digest: digest("cccccccccccc")}},
		},
	}
}

// replicationFixture serves replicatedHarbor to namespaces ns1 and ns2, and links its artifacts to the replications in replications.
type replicationFixture struct {
	*fixture
	replications *fakeReplicationHarbor
	namespaces   namespaceObjects
}

// newUnreadReplicationFixture returns a fixture that has not read Harbor yet, whose replications hold ns1/nginx.
func newUnreadReplicationFixture() *replicationFixture {
	h, r := replicatedHarbor(), &fakeReplicationHarbor{}
	r.put(*storedPolicy())
	n := namespaceObjects{"ns1": namespace("ns1"), "ns2": namespace("ns2")}
	s := NewStore("proj", stalenessLimit, n)
	c := testingclock.NewFakeClock(time.Now())
	s.clock = c
	p := NewPoller(h, s)
	p.LinkReplications(r, replicationConfig)
	return &replicationFixture{&fixture{harbor: h, clock: c, poller: p, repositories: NewRepositories(s), artifacts: NewArtifacts(s)}, r, n}
}

func newReplicationFixture(t *testing.T) *replicationFixture {
	t.Helper()
	f := newUnreadReplicationFixture()
	f.poll(t)
	return f
}

// replicationLinks describes each artifact's replication label and ownerReferences.
func replicationLinks(items []v1alpha1.HarborArtifact) []string {
	var out []string
	for _, a := range items {
		link := a.Namespace + "/" + a.Name
		if label, ok := a.Labels[v1alpha1.ReplicationLabel]; ok {
			link += " label " + label
		}
		for _, o := range a.OwnerReferences {
			link += fmt.Sprintf(" owner %s %s %s %s", o.APIVersion, o.Kind, o.Name, o.UID)
		}
		out = append(out, link)
	}
	return out
}

func byReplication(name string) *metainternalversion.ListOptions {
	return &metainternalversion.ListOptions{LabelSelector: labels.SelectorFromSet(labels.Set{v1alpha1.ReplicationLabel: name})}
}

func TestReplicatedArtifactsLinkToTheirReplicationInItsNamespace(t *testing.T) {
	f := newReplicationFixture(t)
	want := []string{
		"ns1/k8s.ns1.nginx-old.library.nginx.sha256-bbbbbbbbbbbb",
		"ns1/" + replicatedArtifact + " label nginx owner harbor.goharbor.io/v1alpha1 HarborReplication nginx nginx-uid",
		"ns1/nginx.sha256-cccccccccccc",
		"ns2/k8s.ns1.nginx-old.library.nginx.sha256-bbbbbbbbbbbb",
		"ns2/" + replicatedArtifact,
		"ns2/nginx.sha256-cccccccccccc",
	}
	items := artifactItems(t, f.artifacts, "", nil)
	if diff := cmp.Diff(want, replicationLinks(items)); diff != "" {
		t.Errorf("links (-want +got):\n%s", diff)
	}

	wantMeta := metav1.ObjectMeta{
		Name:      replicatedArtifact,
		Namespace: "ns1",
		UID:       uid("ns1", "harborartifacts", 1),
		Labels:    map[string]string{v1alpha1.RepositoryLabel: "k8s.ns1.nginx.library.nginx", v1alpha1.ReplicationLabel: "nginx"},
		OwnerReferences: []metav1.OwnerReference{
			{APIVersion: "harbor.goharbor.io/v1alpha1", Kind: "HarborReplication", Name: "nginx", UID: "nginx-uid"},
		},
	}
	if diff := cmp.Diff(wantMeta, items[1].ObjectMeta); diff != "" {
		t.Errorf("metadata (-want +got):\n%s", diff)
	}
	for _, want := range items {
		got, err := f.artifacts.Get(inNamespace(want.Namespace), want.Name, &metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(&want, got); diff != "" {
			t.Errorf("%s/%s: get differs from list (-list +get):\n%s", want.Namespace, want.Name, diff)
		}
	}
	if diff := cmp.Diff([]string{"ListReplicationPolicies k8s.proj."}, f.replications.calls); diff != "" {
		t.Errorf("replication calls (-want +got):\n%s", diff)
	}
}

func TestReplicatedArtifactsAreCopies(t *testing.T) {
	f := newReplicationFixture(t)
	obj, err := f.artifacts.Get(inNamespace("ns1"), replicatedArtifact, &metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a := obj.(*v1alpha1.HarborArtifact)
	if len(a.OwnerReferences) != 1 {
		t.Fatalf("got ownerReferences %v", a.OwnerReferences)
	}
	a.Labels[v1alpha1.RepositoryLabel] = "changed"
	a.OwnerReferences[0].Name = "changed"
	if diff := cmp.Diff([]string{"ns1/" + replicatedArtifact + " label nginx owner harbor.goharbor.io/v1alpha1 HarborReplication nginx nginx-uid"}, replicationLinks(artifactItems(t, f.artifacts, "ns1", byReplication("nginx")))); diff != "" {
		t.Errorf("links (-want +got):\n%s", diff)
	}
	for _, item := range artifactItems(t, f.artifacts, "", nil) {
		if label := item.Labels[v1alpha1.RepositoryLabel]; label == "changed" {
			t.Errorf("%s/%s has repository label %q", item.Namespace, item.Name, label)
		}
	}
}

func TestRecreatedNamespaceSeesNoReplicationLinks(t *testing.T) {
	f := newReplicationFixture(t)
	f.namespaces["ns1"] = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns1", UID: "recreated"}}
	items := artifactItems(t, f.artifacts, "ns1", nil)
	if diff := cmp.Diff(artifactNames(items), replicationLinks(items)); diff != "" {
		t.Errorf("links (-none +got):\n%s", diff)
	}
	obj, err := f.artifacts.Get(inNamespace("ns1"), replicatedArtifact, &metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a := obj.(*v1alpha1.HarborArtifact); a.Labels[v1alpha1.ReplicationLabel] != "" || a.OwnerReferences != nil {
		t.Errorf("get has labels %v and ownerReferences %v", a.Labels, a.OwnerReferences)
	}
	if names := artifactNames(artifactItems(t, f.artifacts, "", byReplication("nginx"))); names != nil {
		t.Errorf("listed %v by the replication label", names)
	}
}

func TestListArtifactsByReplicationLabel(t *testing.T) {
	f := newReplicationFixture(t)
	for _, tc := range []struct {
		desc, namespace string
		opts            *metainternalversion.ListOptions
		want            []string
	}{
		{"replication", "ns1", byReplication("nginx"), []string{"ns1/" + replicatedArtifact}},
		{"replication across namespaces", "", byReplication("nginx"), []string{"ns1/" + replicatedArtifact}},
		{"replication from another namespace", "ns2", byReplication("nginx"), nil},
		{"another replication", "ns1", byReplication("nginx-old"), nil},
		{"replication and repository", "ns1", byLabelSelector(v1alpha1.ReplicationLabel + "=nginx," + v1alpha1.RepositoryLabel + "=k8s.ns1.nginx.library.nginx"), []string{"ns1/" + replicatedArtifact}},
		{"no replication", "", byLabelSelector("!" + v1alpha1.ReplicationLabel), []string{
			"ns1/k8s.ns1.nginx-old.library.nginx.sha256-bbbbbbbbbbbb",
			"ns1/nginx.sha256-cccccccccccc",
			"ns2/k8s.ns1.nginx-old.library.nginx.sha256-bbbbbbbbbbbb",
			"ns2/" + replicatedArtifact,
			"ns2/nginx.sha256-cccccccccccc",
		}},
	} {
		if diff := cmp.Diff(tc.want, artifactNames(artifactItems(t, f.artifacts, tc.namespace, tc.opts))); diff != "" {
			t.Errorf("%s: names (-want +got):\n%s", tc.desc, diff)
		}
	}
}
