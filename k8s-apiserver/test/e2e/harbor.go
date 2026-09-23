// Package e2e seeds Harbor and tests a deployed harbor-apiserver.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	robotName          = "harbor-apiserver"
)

var adminAuth = authn.Basic{Username: "admin", Password: "Harbor12345"}

// requestTimeout bounds each Harbor API call.
var requestTimeout = 30 * time.Second

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

// ReplaceRepositories creates the e2e project if needed and replaces its repositories with the seed images.
func (a *Admin) ReplaceRepositories(ctx context.Context, s Seed) error {
	if err := a.do(ctx, http.MethodPost, "/projects", map[string]any{"project_name": HarborProject}, nil, http.StatusCreated, http.StatusConflict); err != nil {
		return err
	}
	if err := a.deleteRepositories(ctx); err != nil {
		return err
	}
	untagged, err := s.Untagged.Digest()
	if err != nil {
		return err
	}
	for _, p := range []struct {
		ref string
		t   remote.Taggable
	}{
		{"app:v1", s.App},
		{"app:latest", s.App},
		{"app@" + untagged.String(), s.Untagged},
		{"multi:v1", s.Multi},
		{"team/api:v1", s.TeamAPI},
		{"dotted.name_x:v1", s.Dotted},
	} {
		if err := a.push(ctx, p.ref, p.t); err != nil {
			return fmt.Errorf("pushing %s: %w", p.ref, err)
		}
	}
	return nil
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

// Admin calls Harbor as its admin user.
type Admin struct {
	url  string
	http *http.Client
}

func NewAdmin(harborURL string) *Admin {
	return &Admin{url: harborURL, http: &http.Client{Timeout: requestTimeout}}
}

// push pushes to ref in the e2e project.
func (a *Admin) push(ctx context.Context, ref string, t remote.Taggable) error {
	r, err := name.ParseReference(strings.TrimPrefix(a.url, "http://")+"/"+HarborProject+"/"+ref, name.Insecure)
	if err != nil {
		return err
	}
	return remote.Push(r, t, remote.WithAuth(&adminAuth), remote.WithContext(ctx))
}

// deleteRepositories rereads the first page of repositories until it is empty, because deleting shifts the pages.
func (a *Admin) deleteRepositories(ctx context.Context) error {
	for {
		var repos []struct{ Name string }
		if err := a.do(ctx, http.MethodGet, "/projects/"+HarborProject+"/repositories?page_size=100", nil, &repos, http.StatusOK); err != nil {
			return err
		}
		if len(repos) == 0 {
			return nil
		}
		for _, r := range repos {
			// Harbor requires repository names containing "/" to be encoded twice.
			escaped := url.PathEscape(url.PathEscape(strings.TrimPrefix(r.Name, HarborProject+"/")))
			if err := a.do(ctx, http.MethodDelete, "/projects/"+HarborProject+"/repositories/"+escaped, nil, nil, http.StatusOK); err != nil {
				return err
			}
		}
	}
}

// CreateRobot replaces the project robot account for harbor-apiserver, granting only the permissions it needs.
// It returns the robot's full name and secret.
func (a *Admin) CreateRobot(ctx context.Context) (name, secret string, err error) {
	if err := a.deleteRobot(ctx); err != nil {
		return "", "", err
	}
	access := []map[string]string{
		{"resource": "repository", "action": "list"},
		{"resource": "repository", "action": "read"},
		{"resource": "artifact", "action": "list"},
	}
	body := map[string]any{
		"name":     robotName,
		"level":    "project",
		"duration": -1,
		"permissions": []map[string]any{
			{"kind": "project", "namespace": HarborProject, "access": access},
		},
	}
	var robot struct{ Name, Secret string }
	err = a.do(ctx, http.MethodPost, "/robots", body, &robot, http.StatusCreated)
	return robot.Name, robot.Secret, err
}

func (a *Admin) deleteRobot(ctx context.Context) error {
	var project struct {
		ID int64 `json:"project_id"`
	}
	if err := a.do(ctx, http.MethodGet, "/projects/"+HarborProject, nil, &project, http.StatusOK); err != nil {
		return err
	}
	// Harbor stores the name as "<project>+<robot>" and unescapes q once more after parsing the query string.
	q := fmt.Sprintf("Level=project,ProjectID=%d,name=%s", project.ID, url.QueryEscape(HarborProject+"+"+robotName))
	var robots []struct{ ID int64 }
	if err := a.do(ctx, http.MethodGet, "/robots?"+url.Values{"q": {q}}.Encode(), nil, &robots, http.StatusOK); err != nil {
		return err
	}
	for _, r := range robots {
		if err := a.do(ctx, http.MethodDelete, fmt.Sprintf("/robots/%d", r.ID), nil, nil, http.StatusOK); err != nil {
			return err
		}
	}
	return nil
}

func (a *Admin) do(ctx context.Context, method, path string, body, into any, okStatus ...int) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.url+"/api/v2.0"+path, reqBody)
	if err != nil {
		return err
	}
	req.SetBasicAuth(adminAuth.Username, adminAuth.Password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
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
