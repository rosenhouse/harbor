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

const (
	sha256Digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	sha512Digest = "sha512:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

func TestArtifactObjectName(t *testing.T) {
	for _, tc := range []struct {
		repository, digest, want string
	}{
		{"team/api", sha256Digest, "team.api.sha256-0123456789ab"},
		{"dotted.name", sha256Digest, repositoryObjectName("dotted.name") + ".sha256-0123456789ab"},
		{"nginx", sha512Digest, "nginx.sha512-fedcba987654"},
		{"nginx", "sha256:0123456789a", ""},
		{"nginx", "sha256:0123456789AB", ""},
		{"nginx", "SHA256:0123456789ab", ""},
		{"nginx", "sha.256:0123456789ab", ""},
		{"nginx", "0123456789ab", ""},
		{"nginx", "", ""},
	} {
		if got := artifactObjectName(tc.repository, tc.digest); got != tc.want {
			t.Errorf("%q, %q: got %q, want %q", tc.repository, tc.digest, got, tc.want)
		}
	}
}

func TestLongArtifactObjectNames(t *testing.T) {
	fits := strings.Repeat("a", validation.DNS1123SubdomainMaxLength-len(".sha256-0123456789ab"))
	if name := artifactObjectName(fits, sha256Digest); name != fits+".sha256-0123456789ab" {
		t.Errorf("%d-character repository: got %q", len(fits), name)
	}

	names := map[string]string{}
	for _, repository := range []string{
		fits,
		fits + "a",
		fits + "a/b",
		fits + "a/c",
		strings.Repeat("a", validation.DNS1123SubdomainMaxLength),
		strings.Repeat("a", 300),
		strings.Repeat("a", 300) + ".b",
		strings.Repeat("a", 300) + ".c",
	} {
		for _, digest := range []string{sha256Digest, sha512Digest} {
			name := artifactObjectName(repository, digest)
			if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
				t.Errorf("%d-character repository: %q is not a valid name: %v", len(repository), name, errs)
			}
			if other, ok := names[name]; ok {
				t.Errorf("%q and %q both map to %q", repository, other, name)
			}
			names[name] = repository
		}
	}
}

func TestNamesArtifactOf(t *testing.T) {
	fits := strings.Repeat("a", validation.DNS1123SubdomainMaxLength-len(".sha256-0123456789ab"))
	repositories := []string{"team", "team/api", "dotted.name", fits, fits + "a", fits + "b"}
	for _, of := range repositories {
		for _, digest := range []string{sha256Digest, "b:0123456789ab"} {
			name := artifactObjectName(of, digest)
			for _, repository := range repositories {
				if got := namesArtifactOf(name, repository); got != (repository == of) {
					t.Errorf("%q names an artifact of %q: got %v", name, repository, got)
				}
			}
		}
	}
	for _, name := range []string{"team", "", "team.sha256-0123456789ab.x", "x." + strings.Repeat("a", 250), "x." + strings.Repeat("a", 300)} {
		if namesArtifactOf(name, "team") {
			t.Errorf("%q names an artifact of team", name)
		}
	}
}
