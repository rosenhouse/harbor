package registry

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestRepositoryObjectName(t *testing.T) {
	long := strings.Repeat("a", 260) + "/b"
	for _, tc := range []struct {
		repository string
		want       string
	}{
		{"nginx", "nginx"},
		{"team/api", "team.api"},
		{"team/sub-team/api", "team.sub-team.api"},
		{"a--b", "a--b"},
		{"dotted.name", "dotted.name-"},
		{"under_score", "under-score-"},
		{"double__under", "double--under-"},
		{"team/dotted.name", "team.dotted.name-"},
		{long, strings.Repeat("a", 242) + "-"},
		{strings.Repeat("a", 241) + "/" + strings.Repeat("b", 20), strings.Repeat("a", 241) + "-"},
		{"looks/hashed-0123456789", "looks.hashed-0123456789-"},
	} {
		got := repositoryObjectName(tc.repository)
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("%q: got %q, want prefix %q", tc.repository, got, tc.want)
		}
		if strings.HasSuffix(tc.want, "-") && len(got) != len(tc.want)+hashLength {
			t.Errorf("%q: got %q, want a %d-character hash after %q", tc.repository, got, hashLength, tc.want)
		}
		if errs := validation.IsDNS1123Subdomain(got); len(errs) > 0 {
			t.Errorf("%q: %q is not a valid name: %v", tc.repository, got, errs)
		}
	}
}

func TestRepositoryObjectNamesAreDistinct(t *testing.T) {
	names := map[string]string{}
	hashOfDotted := repositoryObjectName("foo.bar")[len("foo.bar-"):]
	for _, repo := range []string{"a.b", "a/b", "a_b", "a-b", "a__b", "a--b", "a/b.c", "a.b/c", "foo.bar", "foo/bar-" + hashOfDotted} {
		n := repositoryObjectName(repo)
		if other, ok := names[n]; ok {
			t.Errorf("%q and %q both map to %q", repo, other, n)
		}
		names[n] = repo
	}
}

func TestRepositoryNameCandidate(t *testing.T) {
	for _, repo := range []string{"nginx", "team/api", "team/sub-team/api"} {
		if got := repositoryNameCandidate(repositoryObjectName(repo)); got != repo {
			t.Errorf("%q: round trip gave %q", repo, got)
		}
	}
}

func TestHashed(t *testing.T) {
	for _, repo := range []string{"nginx", "team/api", "a/b-abcdef0123x"} {
		if name := repositoryObjectName(repo); isHashed(name) {
			t.Errorf("%q maps to %q, which looks hashed", repo, name)
		}
	}
	for _, repo := range []string{"dotted.name", "under_score", "a-0123456789"} {
		if name := repositoryObjectName(repo); !isHashed(name) {
			t.Errorf("%q maps to %q, which does not look hashed", repo, name)
		}
	}
}
