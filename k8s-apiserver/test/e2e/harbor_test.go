//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

// robotClient reads Harbor as the robot that the seed command stored for harbor-apiserver.
func robotClient(t *testing.T, password string) *harbor.Client {
	t.Helper()
	username := secretValue(t, "username")
	if password == "" {
		password = secretValue(t, "password")
	}
	c, err := harbor.NewClient(HarborURL, username, password, &http.Client{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func secretValue(t *testing.T, key string) string {
	t.Helper()
	b64 := mustKubectl(t, "-n", "harbor-apiserver", "get", "secret", "harbor-apiserver", "-o", "jsonpath={.data."+key+"}")
	v, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	return string(v)
}

func TestRobotReadsSeededProject(t *testing.T) {
	ctx := context.Background()
	c := robotClient(t, "")
	seed := NewSeed()

	repos, err := c.ListRepositories(ctx, HarborProject)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range repos {
		names = append(names, r.Name)
	}
	slices.Sort(names)
	if diff := cmp.Diff([]string{"e2e/app", "e2e/dotted.name_x", "e2e/multi", "e2e/team/api"}, names); diff != "" {
		t.Errorf("repositories (-want +got):\n%s", diff)
	}

	repo, err := c.GetRepository(ctx, HarborProject, "team/api")
	if err != nil {
		t.Fatal(err)
	}
	if repo.Name != "e2e/team/api" || repo.ArtifactCount != 1 {
		t.Errorf("team/api: got %+v", repo)
	}

	for _, tc := range []struct {
		repo string
		want []artifactSummary
	}{
		{"app", []artifactSummary{
			{Digest: Digest(seed.App), Tags: []string{"latest", "v1"}},
			{Digest: Digest(seed.Untagged)},
		}},
		{"multi", []artifactSummary{{
			Digest:     Digest(seed.Multi),
			Tags:       []string{"v1"},
			References: []string{"amd64=" + Digest(seed.MultiAMD64), "arm64=" + Digest(seed.MultiARM64)},
		}}},
		{"team/api", []artifactSummary{{Digest: Digest(seed.TeamAPI), Tags: []string{"v1"}}}},
		{"dotted.name_x", []artifactSummary{{Digest: Digest(seed.Dotted), Tags: []string{"v1"}}}},
	} {
		artifacts, err := c.ListArtifacts(ctx, HarborProject, tc.repo)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(sortedSummaries(tc.want), summarize(artifacts)); diff != "" {
			t.Errorf("%s artifacts (-want +got):\n%s", tc.repo, diff)
		}
	}

	a, err := c.GetArtifact(ctx, HarborProject, "team/api", Digest(seed.TeamAPI))
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest != Digest(seed.TeamAPI) || a.RepositoryName != "e2e/team/api" {
		t.Errorf("team/api artifact: got %+v", a)
	}
}

func TestRobotWithWrongPassword(t *testing.T) {
	_, err := robotClient(t, "wrong").ListRepositories(context.Background(), HarborProject)
	if !errors.Is(err, harbor.ErrUnauthorized) {
		t.Errorf("got %v, want %v", err, harbor.ErrUnauthorized)
	}
}

type artifactSummary struct {
	Digest     string
	Tags       []string
	References []string
}

func summarize(artifacts []harbor.Artifact) []artifactSummary {
	var s []artifactSummary
	for _, a := range artifacts {
		x := artifactSummary{Digest: a.Digest}
		for _, tag := range a.Tags {
			x.Tags = append(x.Tags, tag.Name)
		}
		slices.Sort(x.Tags)
		for _, r := range a.References {
			x.References = append(x.References, r.Platform.Architecture+"="+r.ChildDigest)
		}
		slices.Sort(x.References)
		s = append(s, x)
	}
	return sortedSummaries(s)
}

func sortedSummaries(s []artifactSummary) []artifactSummary {
	slices.SortFunc(s, func(a, b artifactSummary) int { return strings.Compare(a.Digest, b.Digest) })
	return s
}
