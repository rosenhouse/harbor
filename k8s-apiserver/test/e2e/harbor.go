//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

const (
	// HarborURL is Harbor's NodePort, which hack/e2e/kind.yaml maps to the host.
	HarborURL = "http://localhost:30002"
	// HarborInClusterURL is how harbor-apiserver reaches Harbor.
	HarborInClusterURL = "http://harbor.harbor.svc"
	HarborProject      = "e2e"
)

var admin = authn.Basic{Username: "admin", Password: "Harbor12345"}

// Seed is what Harbor holds after seeding. Its images are reproducible, so tests can recompute their digests.
type Seed struct {
	App, Untagged, TeamAPI, Dotted v1.Image
	Multi                          v1.ImageIndex
	MultiAMD64, MultiARM64         v1.Image
}

func NewSeed() Seed {
	s := Seed{
		App:        image("app", "amd64"),
		Untagged:   image("untagged", "amd64"),
		TeamAPI:    image("team/api", "amd64"),
		Dotted:     image("dotted.name_x", "amd64"),
		MultiAMD64: image("multi", "amd64"),
		MultiARM64: image("multi", "arm64"),
	}
	s.Multi = mutate.AppendManifests(mutate.IndexMediaType(empty.Index, types.OCIImageIndex),
		mutate.IndexAddendum{Add: s.MultiAMD64, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
		mutate.IndexAddendum{Add: s.MultiARM64, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}},
	)
	return s
}

// Push creates the project and pushes the seed images to it.
func (s Seed) Push() error {
	if err := harborAdmin(http.MethodPost, "/projects", map[string]any{"project_name": HarborProject}, nil, http.StatusCreated, http.StatusConflict); err != nil {
		return err
	}
	for _, p := range []struct {
		ref string
		t   remote.Taggable
	}{
		{"app:v1", s.App},
		{"app:latest", s.App},
		{"app@", s.Untagged},
		{"multi:v1", s.Multi},
		{"team/api:v1", s.TeamAPI},
		{"dotted.name_x:v1", s.Dotted},
	} {
		if err := push(p.ref, p.t); err != nil {
			return fmt.Errorf("pushing %s: %w", p.ref, err)
		}
	}
	return nil
}

// Digest returns the digest of an image or index, panicking on error.
func Digest(t interface{ Digest() (v1.Hash, error) }) string {
	d, err := t.Digest()
	if err != nil {
		panic(err)
	}
	return d.String()
}

// image returns a small image whose digest depends only on its arguments.
func image(content, arch string) v1.Image {
	img, err := mutate.AppendLayers(
		mutate.ConfigMediaType(mutate.MediaType(empty.Image, types.OCIManifestSchema1), types.OCIConfigJSON),
		static.NewLayer([]byte(content), types.OCILayer))
	if err != nil {
		panic(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		panic(err)
	}
	cf.OS, cf.Architecture = "linux", arch
	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		panic(err)
	}
	return img
}

// push pushes to repoRef in the e2e project. A reference ending in "@" pushes by digest, leaving the artifact untagged.
func push(repoRef string, t remote.Taggable) error {
	s := strings.TrimPrefix(HarborURL, "http://") + "/" + HarborProject + "/" + repoRef
	if strings.HasSuffix(s, "@") {
		s += Digest(t.(interface{ Digest() (v1.Hash, error) }))
	}
	ref, err := name.ParseReference(s, name.Insecure)
	if err != nil {
		return err
	}
	auth := remote.WithAuth(&admin)
	switch t := t.(type) {
	case v1.ImageIndex:
		return remote.WriteIndex(ref, t, auth)
	case v1.Image:
		return remote.Write(ref, t, auth)
	}
	return fmt.Errorf("cannot push %T", t)
}

// CreateRobot creates a project robot account with only the permissions harbor-apiserver needs.
// It returns the robot's full name and secret.
func CreateRobot() (name, secret string, err error) {
	access := []map[string]string{
		{"resource": "repository", "action": "list"},
		{"resource": "repository", "action": "read"},
		{"resource": "artifact", "action": "list"},
		{"resource": "artifact", "action": "read"},
	}
	body := map[string]any{
		"name":     fmt.Sprintf("k8s-%d", time.Now().Unix()),
		"level":    "project",
		"duration": -1,
		"permissions": []map[string]any{
			{"kind": "project", "namespace": HarborProject, "access": access},
		},
	}
	var robot struct{ Name, Secret string }
	err = harborAdmin(http.MethodPost, "/robots", body, &robot, http.StatusCreated)
	return robot.Name, robot.Secret, err
}

func harborAdmin(method, path string, body, into any, okStatus ...int) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(method, HarborURL+"/api/v2.0"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.SetBasicAuth(admin.Username, admin.Password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if !slices.Contains(okStatus, resp.StatusCode) {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, respBody)
	}
	if into != nil {
		return json.Unmarshal(respBody, into)
	}
	return nil
}
