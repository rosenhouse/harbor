package e2e

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

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

func TestSummarizeReferenceWithoutPlatform(t *testing.T) {
	got := summarize([]harbor.Artifact{{
		Digest:     "sha256:index",
		References: []harbor.Reference{{ChildDigest: "sha256:child"}},
	}})

	want := []artifactSummary{{Digest: "sha256:index", References: []string{"<no platform>=sha256:child"}}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}
