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
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

// robotClient reads Harbor as the robot that the seed command stored for harbor-apiserver.
func robotClient(t *testing.T) *harbor.Client {
	t.Helper()
	return clientAs(t, secretValue(t, "username"), secretValue(t, "password"))
}

func clientAs(t *testing.T, username, password string) *harbor.Client {
	t.Helper()
	c, err := harbor.NewClient(HarborURL, username, password, &http.Client{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func digest(t *testing.T, x interface{ Digest() (v1.Hash, error) }) string {
	t.Helper()
	d, err := x.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d.String()
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
	c := robotClient(t)
	seed := NewSeed()

	repos, err := c.ListRepositories(ctx, HarborProject)
	if err != nil {
		t.Fatal(err)
	}
	artifactCounts := map[string]int64{}
	for _, r := range repos {
		artifactCounts[r.Name] = r.ArtifactCount
	}
	// Untagged artifacts count, but an index's children don't.
	wantCounts := map[string]int64{"e2e/app": 2, "e2e/dotted.name_x": 1, "e2e/multi": 1, "e2e/team/api": 1}
	if diff := cmp.Diff(wantCounts, artifactCounts); diff != "" {
		t.Errorf("artifact counts by repository (-want +got):\n%s", diff)
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
			{Digest: digest(t, seed.App), Tags: []string{"latest", "v1"}},
			{Digest: digest(t, seed.Untagged)},
		}},
		{"multi", []artifactSummary{{
			Digest:     digest(t, seed.Multi),
			Tags:       []string{"v1"},
			References: []string{"amd64=" + digest(t, seed.MultiAMD64), "arm64=" + digest(t, seed.MultiARM64)},
		}}},
		{"team/api", []artifactSummary{{Digest: digest(t, seed.TeamAPI), Tags: []string{"v1"}}}},
		{"dotted.name_x", []artifactSummary{{Digest: digest(t, seed.Dotted), Tags: []string{"v1"}}}},
	} {
		artifacts, err := c.ListArtifacts(ctx, HarborProject, tc.repo)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(sortedSummaries(tc.want), summarize(artifacts)); diff != "" {
			t.Errorf("%s artifacts (-want +got):\n%s", tc.repo, diff)
		}
	}

	a, err := c.GetArtifact(ctx, HarborProject, "team/api", digest(t, seed.TeamAPI))
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest != digest(t, seed.TeamAPI) || a.RepositoryName != "e2e/team/api" {
		t.Errorf("team/api artifact: got %+v", a)
	}
}

func TestRobotWithWrongPassword(t *testing.T) {
	_, err := clientAs(t, secretValue(t, "username"), "wrong").ListRepositories(context.Background(), HarborProject)
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
			arch := "<no platform>"
			if r.Platform != nil {
				arch = r.Platform.Architecture
			}
			x.References = append(x.References, arch+"="+r.ChildDigest)
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
