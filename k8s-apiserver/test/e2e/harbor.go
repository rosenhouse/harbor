// Package e2e seeds Harbor and tests a deployed harbor-apiserver.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
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
	// SourceProject holds the images that replications copy.
	SourceProject = "e2e-source"
	// ReplicationRegistry is the registry endpoint that replications may copy from. It is Harbor itself.
	ReplicationRegistry = "e2e-harbor"
	// unlistedRegistry is a registry endpoint that replications may not copy from.
	unlistedRegistry     = "e2e-unlisted"
	robotName            = "harbor-apiserver"
	replicationRobotName = "harbor-apiserver-replication"
	// replicationPrefix is harbor-apiserver's default --replication-prefix.
	replicationPrefix = "k8s"
)

var adminAuth = authn.Basic{Username: "admin", Password: "Harbor12345"}

// requestTimeout bounds each Harbor API call.
var requestTimeout = 30 * time.Second

// stopRetryInterval is how often a policy delete retries while the policy's executions stop.
var stopRetryInterval = time.Second

// Seed is what Harbor holds after seeding. Its images are reproducible, so tests can recompute their digests.
type Seed struct {
	App, Untagged, TeamAPI, Dotted v1.Image
	Multi                          v1.ImageIndex
	MultiAMD64, MultiARM64         v1.Image
	// SourceV1 and SourceV2 are tags v1 and v2 of team/app in the source project.
	SourceV1, SourceV2 v1.Image
}

func NewSeed() Seed {
	s := Seed{
		App:        image("app", "amd64"),
		Untagged:   image("untagged", "amd64"),
		TeamAPI:    image("team/api", "amd64"),
		Dotted:     image("dotted.name_x", "amd64"),
		MultiAMD64: image("multi", "amd64"),
		MultiARM64: image("multi", "arm64"),
		SourceV1:   image("source v1", "amd64"),
		SourceV2:   image("source v2", "amd64"),
	}
	s.Multi = mutate.AppendManifests(mutate.IndexMediaType(empty.Index, types.OCIImageIndex),
		mutate.IndexAddendum{Add: s.MultiAMD64, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
		mutate.IndexAddendum{Add: s.MultiARM64, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}},
	)
	return s
}

// ReplaceRepositories creates the e2e and source projects if needed, and replaces their repositories with the seed images.
// It first deletes the policies that could replicate into the e2e project.
func (a *Admin) ReplaceRepositories(ctx context.Context, s Seed) error {
	if err := a.deleteReplicationPolicies(ctx, replicationPrefix+"."+HarborProject+"."); err != nil {
		return err
	}
	untagged, err := s.Untagged.Digest()
	if err != nil {
		return err
	}
	type pushed struct {
		ref string
		t   remote.Taggable
	}
	for _, p := range []struct {
		name   string
		images []pushed
	}{
		{HarborProject, []pushed{
			{"app:v1", s.App},
			{"app:latest", s.App},
			{"app@" + untagged.String(), s.Untagged},
			{"multi:v1", s.Multi},
			{"team/api:v1", s.TeamAPI},
			{"dotted.name_x:v1", s.Dotted},
		}},
		{SourceProject, []pushed{
			{"team/app:v1", s.SourceV1},
			{"team/app:v2", s.SourceV2},
		}},
	} {
		if err := a.do(ctx, http.MethodPost, "/projects", map[string]any{"project_name": p.name}, nil, http.StatusCreated, http.StatusConflict); err != nil {
			return err
		}
		if err := a.deleteRepositories(ctx, p.name, ""); err != nil {
			return err
		}
		for _, i := range p.images {
			if err := a.push(ctx, p.name+"/"+i.ref, i.t); err != nil {
				return fmt.Errorf("pushing %s/%s: %w", p.name, i.ref, err)
			}
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

// push pushes to ref, which starts with the project.
func (a *Admin) push(ctx context.Context, ref string, t remote.Taggable) error {
	r, err := name.ParseReference(strings.TrimPrefix(a.url, "http://")+"/"+ref, name.Insecure)
	if err != nil {
		return err
	}
	return remote.Push(r, t, remote.WithAuth(&adminAuth), remote.WithContext(ctx))
}

// deleteRepositories deletes the repositories of a project whose names, within the project, start with prefix.
// It rereads the first page until it finds none to delete, because deleting shifts the pages.
func (a *Admin) deleteRepositories(ctx context.Context, project, prefix string) error {
	for {
		// Harbor matches names that contain the value, and unescapes q once more after parsing the query string.
		query := url.Values{"page_size": {"100"}, "q": {"name=~" + url.QueryEscape(project+"/"+prefix)}}
		var repos []struct{ Name string }
		if err := a.do(ctx, http.MethodGet, "/projects/"+project+"/repositories?"+query.Encode(), nil, &repos, http.StatusOK); err != nil {
			return err
		}
		deleted := 0
		for _, r := range repos {
			repository, ok := strings.CutPrefix(r.Name, project+"/"+prefix)
			if !ok {
				continue
			}
			if err := a.deleteRepository(ctx, project, prefix+repository); err != nil {
				return err
			}
			deleted++
		}
		if deleted == 0 {
			return nil
		}
	}
}

// deleteRepository deletes a repository, named within its project.
func (a *Admin) deleteRepository(ctx context.Context, project, repository string) error {
	// Harbor requires repository names containing "/" to be encoded twice.
	escaped := url.PathEscape(url.PathEscape(repository))
	return a.do(ctx, http.MethodDelete, "/projects/"+project+"/repositories/"+escaped, nil, nil, http.StatusOK)
}

// deleteReplications deletes the policies that harbor-apiserver created for a namespace, and the repositories they copied into.
func (a *Admin) deleteReplications(ctx context.Context, namespace string) error {
	if err := a.deleteReplicationPolicies(ctx, replicationPrefix+"."+HarborProject+"."+namespace+"."); err != nil {
		return err
	}
	return a.deleteRepositories(ctx, HarborProject, replicationPrefix+"/"+namespace+"/")
}

type replicationPolicy struct {
	ID   int64
	Name string
}

// replicationPolicies returns the policies on the first page of those whose names start with prefix.
func (a *Admin) replicationPolicies(ctx context.Context, prefix string) ([]replicationPolicy, error) {
	query := url.Values{"page_size": {"100"}, "q": {"name=~" + url.QueryEscape(prefix)}}
	var policies []replicationPolicy
	if err := a.do(ctx, http.MethodGet, "/replication/policies?"+query.Encode(), nil, &policies, http.StatusOK); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(policies, func(p replicationPolicy) bool { return !strings.HasPrefix(p.Name, prefix) }), nil
}

// deleteReplicationPolicies deletes the policies whose names start with prefix.
func (a *Admin) deleteReplicationPolicies(ctx context.Context, prefix string) error {
	for {
		policies, err := a.replicationPolicies(ctx, prefix)
		if err != nil || len(policies) == 0 {
			return err
		}
		for _, p := range policies {
			if err := a.deleteReplicationPolicy(ctx, p.ID); err != nil {
				return err
			}
		}
	}
}

// deleteReplicationPolicy stops the policy's running executions until Harbor lets it delete the policy.
func (a *Admin) deleteReplicationPolicy(ctx context.Context, id int64) error {
	for {
		err := a.do(ctx, http.MethodDelete, fmt.Sprintf("/replication/policies/%d", id), nil, nil, http.StatusOK)
		if s := (*statusError)(nil); !errors.As(err, &s) || s.code != http.StatusPreconditionFailed {
			return err
		}
		var running []struct{ ID int64 }
		query := url.Values{"policy_id": {strconv.FormatInt(id, 10)}, "status": {"InProgress"}, "page_size": {"100"}}
		if err := a.do(ctx, http.MethodGet, "/replication/executions?"+query.Encode(), nil, &running, http.StatusOK); err != nil {
			return err
		}
		for _, e := range running {
			if err := a.do(ctx, http.MethodPut, fmt.Sprintf("/replication/executions/%d", e.ID), nil, nil, http.StatusOK); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(stopRetryInterval):
		}
	}
}

// CreateRegistries creates registry endpoints for Harbor itself, if they don't exist: one that replications may copy from, and one that they may not.
func (a *Admin) CreateRegistries(ctx context.Context) error {
	for _, registry := range []string{ReplicationRegistry, unlistedRegistry} {
		body := map[string]any{
			"name":       registry,
			"type":       "harbor",
			"url":        HarborInClusterURL,
			"insecure":   true,
			"credential": map[string]string{"type": "basic", "access_key": adminAuth.Username, "access_secret": adminAuth.Password},
		}
		if err := a.do(ctx, http.MethodPost, "/registries", body, nil, http.StatusCreated, http.StatusConflict); err != nil {
			return err
		}
	}
	return nil
}

// CreateRobot replaces the project robot account for harbor-apiserver, granting only the permissions it needs.
// It returns the robot's full name and secret.
func (a *Admin) CreateRobot(ctx context.Context) (name, secret string, err error) {
	var project struct {
		ID int64 `json:"project_id"`
	}
	if err := a.do(ctx, http.MethodGet, "/projects/"+HarborProject, nil, &project, http.StatusOK); err != nil {
		return "", "", err
	}
	// Harbor stores the name as "<project>+<robot>".
	q := fmt.Sprintf("Level=project,ProjectID=%d,name=%s", project.ID, HarborProject+"+"+robotName)
	access := []map[string]string{
		{"resource": "repository", "action": "list"},
		{"resource": "artifact", "action": "list"},
	}
	return a.replaceRobot(ctx, q, map[string]any{
		"name":     robotName,
		"level":    "project",
		"duration": -1,
		"permissions": []map[string]any{
			{"kind": "project", "namespace": HarborProject, "access": access},
		},
	})
}

// CreateReplicationRobot replaces the system robot account for harbor-apiserver's replications, granting only the permissions that the README lists.
// It returns the robot's full name and secret.
func (a *Admin) CreateReplicationRobot(ctx context.Context) (name, secret string, err error) {
	var access []map[string]string
	for _, p := range []struct {
		resource string
		actions  []string
	}{
		{"registry", []string{"list"}},
		{"replication-policy", []string{"list", "read", "create", "delete"}},
		{"replication", []string{"list", "create"}},
	} {
		for _, action := range p.actions {
			access = append(access, map[string]string{"resource": p.resource, "action": action})
		}
	}
	return a.replaceRobot(ctx, "Level=system,name="+replicationRobotName, map[string]any{
		"name":     replicationRobotName,
		"level":    "system",
		"duration": -1,
		"permissions": []map[string]any{
			{"kind": "system", "namespace": "/", "access": access},
		},
	})
}

// replaceRobot deletes the robots that the query q finds, and creates robot. It returns the new robot's full name and secret.
func (a *Admin) replaceRobot(ctx context.Context, q string, robot map[string]any) (name, secret string, err error) {
	var robots []struct{ ID int64 }
	// Harbor unescapes q once more after parsing the query string.
	if err := a.do(ctx, http.MethodGet, "/robots?"+url.Values{"q": {url.QueryEscape(q)}}.Encode(), nil, &robots, http.StatusOK); err != nil {
		return "", "", err
	}
	for _, r := range robots {
		if err := a.do(ctx, http.MethodDelete, fmt.Sprintf("/robots/%d", r.ID), nil, nil, http.StatusOK); err != nil {
			return "", "", err
		}
	}
	var created struct{ Name, Secret string }
	err = a.do(ctx, http.MethodPost, "/robots", robot, &created, http.StatusCreated)
	return created.Name, created.Secret, err
}

// statusError is a response with an unexpected status.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

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
		return &statusError{resp.StatusCode, fmt.Sprintf("%s %s: %s: %s", method, path, resp.Status, respBody)}
	}
	if into != nil {
		return json.Unmarshal(respBody, into)
	}
	return nil
}
