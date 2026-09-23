//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

// robotClient reads Harbor as the robot that the seed command stored for harbor-apiserver.
func robotClient(t *testing.T) *harbor.Client {
	t.Helper()
	return clientAs(t, secretValue(t, "username"), secretValue(t, "password"))
}

// seededRepositories are the full names of the repositories that ReplaceRepositories creates, sorted.
var seededRepositories = []string{"e2e/app", "e2e/dotted.name_x", "e2e/multi", "e2e/team/api"}

func clientAs(t *testing.T, username, password string) *harbor.Client {
	t.Helper()
	credentials := func() (string, string, error) { return username, password, nil }
	c, err := harbor.NewClient(HarborURL, credentials, &http.Client{Timeout: 10 * time.Second})
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
		artifacts, err := c.ListArtifacts(ctx, HarborProject, tc.repo, "")
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(sortedSummaries(tc.want), summarize(artifacts)); diff != "" {
			t.Errorf("%s artifacts (-want +got):\n%s", tc.repo, diff)
		}
	}

	digestPrefix := func(digest string) string { return digest[:len("sha256:")+12] }
	for _, tc := range []struct {
		repo, digestPrefix string
		want               []string
	}{
		{"app", digestPrefix(digest(t, seed.Untagged)), []string{digest(t, seed.Untagged)}},
		{"multi", digestPrefix(digest(t, seed.Multi)), []string{digest(t, seed.Multi)}},
		{"multi", digestPrefix(digest(t, seed.MultiAMD64)), nil},
	} {
		artifacts, err := c.ListArtifacts(ctx, HarborProject, tc.repo, tc.digestPrefix)
		if err != nil {
			t.Fatal(err)
		}
		var digests []string
		for _, a := range artifacts {
			digests = append(digests, a.Digest)
		}
		if diff := cmp.Diff(tc.want, digests); diff != "" {
			t.Errorf("%s artifacts with digest prefix %s (-want +got):\n%s", tc.repo, tc.digestPrefix, diff)
		}
	}
}

func TestRobotWithWrongPassword(t *testing.T) {
	_, err := clientAs(t, secretValue(t, "username"), "wrong").ListRepositories(context.Background(), HarborProject)
	if !errors.Is(err, harbor.ErrUnauthorized) {
		t.Errorf("got %v, want %v", err, harbor.ErrUnauthorized)
	}
}
