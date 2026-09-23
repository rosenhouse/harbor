//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
)

// eventuallyKubectl retries kubectl until it succeeds and check accepts its output.
// Each replica has its own namespace informer, so the replica serving a request may not see a new label yet.
func eventuallyKubectl(t *testing.T, check func(out string) error, args ...string) {
	t.Helper()
	eventually(t, func() error {
		out, err := kubectl(t, args...)
		if err != nil {
			return err
		}
		return check(out)
	})
}

// expectArtifacts waits until kubectl lists the artifacts at locations want, and returns them.
func expectArtifacts(t *testing.T, want []string, args ...string) []v1alpha1.HarborArtifact {
	t.Helper()
	var list v1alpha1.HarborArtifactList
	eventuallyKubectl(t, func(out string) error {
		list = v1alpha1.HarborArtifactList{}
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			return err
		}
		if diff := cmp.Diff(want, locations(list.Items)); diff != "" {
			return fmt.Errorf("kubectl get harborartifacts %v (-want +got):\n%s", args, diff)
		}
		return nil
	}, append([]string{"get", "harborartifacts", "-o", "json"}, args...)...)
	return list.Items
}

// locations maps the artifacts to "<full repository name>@<digest>", sorted.
func locations(artifacts []v1alpha1.HarborArtifact) []string {
	var l []string
	for _, a := range artifacts {
		l = append(l, a.Status.Repository+"@"+a.Status.Digest)
	}
	slices.Sort(l)
	return l
}

func artifactName(repositoryObjectName, digest string) string {
	algorithm, hex, _ := strings.Cut(digest, ":")
	return repositoryObjectName + "." + algorithm + "-" + hex[:12]
}

func TestArtifacts(t *testing.T) {
	seed := NewSeed()
	ns := newNamespace(t)
	label(t, ns, HarborProject)
	want := []string{
		"e2e/app@" + digest(t, seed.App),
		"e2e/app@" + digest(t, seed.Untagged),
		"e2e/dotted.name_x@" + digest(t, seed.Dotted),
		"e2e/multi@" + digest(t, seed.Multi),
		"e2e/team/api@" + digest(t, seed.TeamAPI),
	}
	slices.Sort(want)
	artifacts := expectArtifacts(t, want, "-n", ns)
	if t.Failed() {
		t.FailNow()
	}

	repositoryObjectNames := map[string]string{}
	eventuallyKubectl(t, func(out string) error {
		var list v1alpha1.HarborRepositoryList
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			return err
		}
		clear(repositoryObjectNames)
		for _, r := range list.Items {
			repositoryObjectNames[r.Status.Name] = r.Name
		}
		if diff := cmp.Diff(seededRepositories, slices.Sorted(maps.Keys(repositoryObjectNames))); diff != "" {
			return fmt.Errorf("repositories (-want +got):\n%s", diff)
		}
		return nil
	}, "-n", ns, "get", "harborrepositories", "-o", "json")

	byDigest := map[string]v1alpha1.HarborArtifact{}
	for _, a := range artifacts {
		byDigest[a.Status.Digest] = a
		repository := repositoryObjectNames[a.Status.Repository]
		if want := artifactName(repository, a.Status.Digest); a.Name != want {
			t.Errorf("%s: name %q, want %q", a.Status.Digest, a.Name, want)
		}
		if got := a.Labels[v1alpha1.RepositoryLabel]; got != repository {
			t.Errorf("%s: repository label %q, want %q", a.Name, got, repository)
		}
		if a.Namespace != ns || a.UID == "" || a.CreationTimestamp.IsZero() || a.Status.PushTime == nil || a.Status.Type != "IMAGE" || a.Status.Size <= 0 {
			t.Errorf("%s: namespace %q, uid %q, created %v, status %+v", a.Name, a.Namespace, a.UID, a.CreationTimestamp, a.Status)
		}
	}

	teamAPI := byDigest[digest(t, seed.TeamAPI)]
	if want := "team.api.sha256-" + strings.TrimPrefix(digest(t, seed.TeamAPI), "sha256:")[:12]; teamAPI.Name != want {
		t.Errorf("team/api: name %q, want %q", teamAPI.Name, want)
	}
	if teamAPI.Status.MediaType != string(types.OCIManifestSchema1) || teamAPI.Status.ConfigMediaType != string(types.OCIConfigJSON) {
		t.Errorf("team/api: media type %q, config media type %q", teamAPI.Status.MediaType, teamAPI.Status.ConfigMediaType)
	}

	tagNames := func(a v1alpha1.HarborArtifact) []string {
		var names []string
		for _, tag := range a.Status.Tags {
			names = append(names, tag.Name)
		}
		return names
	}
	if tags := tagNames(byDigest[digest(t, seed.App)]); !slices.Equal(tags, []string{"latest", "v1"}) {
		t.Errorf("app: tags %v", tags)
	}
	if tags := tagNames(byDigest[digest(t, seed.Untagged)]); len(tags) != 0 {
		t.Errorf("untagged: tags %v", tags)
	}

	multi := byDigest[digest(t, seed.Multi)]
	wantReferences := []v1alpha1.HarborArtifactReference{
		{Digest: digest(t, seed.MultiAMD64), Platform: &v1alpha1.HarborPlatform{OS: "linux", Architecture: "amd64"}},
		{Digest: digest(t, seed.MultiARM64), Platform: &v1alpha1.HarborPlatform{OS: "linux", Architecture: "arm64"}},
	}
	byDigestOrder := cmpopts.SortSlices(func(a, b v1alpha1.HarborArtifactReference) bool { return a.Digest < b.Digest })
	if diff := cmp.Diff(wantReferences, multi.Status.References, byDigestOrder); diff != "" {
		t.Errorf("multi: references (-want +got):\n%s", diff)
	}
	if multi.Status.MediaType != string(types.OCIImageIndex) || multi.Status.ConfigMediaType != "" {
		t.Errorf("multi: media type %q, config media type %q", multi.Status.MediaType, multi.Status.ConfigMediaType)
	}

	for _, a := range []v1alpha1.HarborArtifact{teamAPI, multi, byDigest[digest(t, seed.Dotted)]} {
		eventuallyKubectl(t, func(out string) error {
			if out != a.Status.Digest {
				return fmt.Errorf("get %s: digest %q", a.Name, out)
			}
			return nil
		}, "-n", ns, "get", "harborartifact", a.Name, "-o", "jsonpath={.status.digest}")
	}
	for _, child := range []string{digest(t, seed.MultiAMD64), digest(t, seed.MultiARM64)} {
		name := artifactName("multi", child)
		if _, err := kubectl(t, "-n", ns, "get", "harborartifact", name); err == nil || !strings.Contains(err.Error(), "NotFound") {
			t.Errorf("get index child %s: %v", name, err)
		}
	}

	for _, filter := range [][]string{
		{"-l", v1alpha1.RepositoryLabel + "=team.api"},
		{"--field-selector", "status.repository=e2e/team/api"},
	} {
		expectArtifacts(t, []string{"e2e/team/api@" + digest(t, seed.TeamAPI)}, append([]string{"-n", ns}, filter...)...)
	}

	eventuallyKubectl(t, func(out string) error {
		header, _, _ := strings.Cut(out, "\n")
		if diff := cmp.Diff([]string{"NAME", "REPOSITORY", "TAGS", "TYPE", "SIZE", "AGE"}, strings.Fields(header)); diff != "" {
			return fmt.Errorf("header (-want +got):\n%s", diff)
		}
		return nil
	}, "-n", ns, "get", "harborartifacts")
	eventuallyKubectl(t, func(out string) error {
		if row := strings.Fields(out); len(row) != 6 || !slices.Equal(row[:4], []string{byDigest[digest(t, seed.App)].Name, "e2e/app", "latest,v1", "IMAGE"}) {
			return fmt.Errorf("app row: %q", row)
		}
		return nil
	}, "-n", ns, "get", "harborartifact", byDigest[digest(t, seed.App)].Name, "--no-headers")
}
