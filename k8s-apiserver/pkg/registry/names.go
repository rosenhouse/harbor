package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

const hashLength = 10

var (
	notNameChars = regexp.MustCompile(`[^a-z0-9.-]`)
	hashSuffix   = regexp.MustCompile(`-[0-9a-f]{10}$`)

	nameableDigest    = regexp.MustCompile(`^([a-z0-9]{1,16}):([0-9a-f]{12})[0-9a-f]*$`)
	artifactNameParts = regexp.MustCompile(`^(.+)(\.([a-z0-9]{1,16})-([0-9a-f]{12}))$`)
)

// repositoryObjectName maps a repository name within its project, such as "team/api", to an object name.
// Slashes become dots, which is reversible when the repository name has no dots of its own.
// Other names get a hash suffix. Reversible names never end in something shaped like one, so the two never collide.
func repositoryObjectName(repository string) string {
	return repositoryObjectNameWithin(repository, validation.DNS1123SubdomainMaxLength)
}

// repositoryObjectNameWithin is repositoryObjectName, hashing names longer than maxLength.
func repositoryObjectNameWithin(repository string, maxLength int) string {
	if !strings.Contains(repository, ".") {
		name := strings.ReplaceAll(repository, "/", ".")
		if len(name) <= maxLength && len(validation.IsDNS1123Subdomain(name)) == 0 && !isHashed(name) {
			return name
		}
	}
	sum := sha256.Sum256([]byte(repository))
	suffix := "-" + hex.EncodeToString(sum[:])[:hashLength]
	base := notNameChars.ReplaceAllString(strings.ReplaceAll(repository, "/", "."), "-")
	base = base[:min(len(base), maxLength-len(suffix))]
	return strings.TrimRight(base, ".-") + suffix
}

// artifactObjectName appends a digest prefix to the repository's object name, such as "team.api.sha256-0123456789ab".
// It shortens the repository part to fit. It returns "" for a digest that cannot be part of a name.
func artifactObjectName(repository, digest string) string {
	m := nameableDigest.FindStringSubmatch(digest)
	if m == nil {
		return ""
	}
	suffix := "." + m[1] + "-" + m[2]
	return repositoryObjectNameWithin(repository, validation.DNS1123SubdomainMaxLength-len(suffix)) + suffix
}

// splitArtifactObjectName returns the repository part of an artifact's object name, the length it was shortened to,
// and the digest prefix, such as "sha256:0123456789ab".
func splitArtifactObjectName(name string) (repositoryPart, digestPrefix string, maxLength int, ok bool) {
	m := artifactNameParts.FindStringSubmatch(name)
	if m == nil {
		return "", "", 0, false
	}
	return m[1], m[3] + ":" + m[4], validation.DNS1123SubdomainMaxLength - len(m[2]), true
}

// isHashed reports whether an object name has a hash suffix, which only listing can resolve.
func isHashed(objectName string) bool {
	return hashSuffix.MatchString(objectName)
}

// repositoryNameCandidate reverses repositoryObjectName for names without a hash suffix.
func repositoryNameCandidate(objectName string) string {
	return strings.ReplaceAll(objectName, ".", "/")
}
