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
)

// repositoryObjectName maps a repository name within its project, such as "team/api", to an object name.
// Slashes become dots, which is reversible when the repository name has no dots of its own.
// Other names get a hash suffix. Reversible names never end in something shaped like one, so the two never collide.
func repositoryObjectName(repository string) string {
	if !strings.Contains(repository, ".") {
		name := strings.ReplaceAll(repository, "/", ".")
		if len(validation.IsDNS1123Subdomain(name)) == 0 && !isHashed(name) {
			return name
		}
	}
	sum := sha256.Sum256([]byte(repository))
	suffix := "-" + hex.EncodeToString(sum[:])[:hashLength]
	base := notNameChars.ReplaceAllString(strings.ReplaceAll(repository, "/", "."), "-")
	base = base[:min(len(base), validation.DNS1123SubdomainMaxLength-len(suffix))]
	return strings.TrimRight(base, ".-") + suffix
}

// isHashed reports whether an object name has a hash suffix, which only listing can resolve.
func isHashed(objectName string) bool {
	return hashSuffix.MatchString(objectName)
}

// repositoryNameCandidate reverses repositoryObjectName for names without a hash suffix.
func repositoryNameCandidate(objectName string) string {
	return strings.ReplaceAll(objectName, ".", "/")
}
