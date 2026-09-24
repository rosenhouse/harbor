package harbor_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

// received is a request that the fake Harbor received.
type received struct {
	Method, URI, ContentType string
	Body                     any
}

// replier answers every request with status, a Location header, and body, and records what it received.
func replier(t *testing.T, status int, location, body string) (*harbor.Client, func() []received) {
	t.Helper()
	var mu sync.Mutex
	var got []received
	f := newFakeHarbor(t, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		mu.Lock()
		got = append(got, received{r.Method, r.RequestURI, r.Header.Get("Content-Type"), decode(t, string(raw))})
		mu.Unlock()
		if location != "" {
			w.Header().Set("Location", location)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	return newClient(t, f), func() []received {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

// decode returns the JSON value of s, or nil if s is empty.
func decode(t *testing.T, s string) any {
	t.Helper()
	if s == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Errorf("decoding %q: %v", s, err)
	}
	return v
}

func onlyGet(uri string) []received { return []received{{Method: http.MethodGet, URI: uri}} }

// dockerHub is a registry as Harbor returns it.
const dockerHub = `{
	"id": 3,
	"name": "dockerhub",
	"type": "docker-hub",
	"url": "https://hub.docker.com",
	"credential": {"type": "basic", "access_key": "user", "access_secret": "*****"},
	"insecure": false,
	"description": "",
	"status": "healthy",
	"creation_time": "2026-01-02T03:04:05.000Z",
	"update_time": "2026-01-02T03:04:05.000Z"
}`

// nginxPolicy is a policy as Harbor's list and get return it.
const nginxPolicy = `{
	"id": 7,
	"name": "k8s.proj.team-a.nginx",
	"description": "{\"managedBy\":\"harbor-apiserver\"}",
	"src_registry": ` + dockerHub + `,
	"dest_registry": {
		"id": 0,
		"name": "Local",
		"type": "harbor",
		"url": "http://core:8080",
		"credential": {"type": "secret", "access_secret": "*****"},
		"insecure": true,
		"status": "healthy",
		"creation_time": "0001-01-01T00:00:00.000Z",
		"update_time": "0001-01-01T00:00:00.000Z"
	},
	"dest_namespace": "proj/k8s/team-a/nginx",
	"dest_namespace_replace_count": 0,
	"trigger": {"type": "scheduled", "trigger_settings": {"cron": "0 30 2 * * *"}},
	"filters": [
		{"type": "name", "value": "library/nginx"},
		{"type": "tag", "value": "1.27", "decoration": "matches"},
		{"type": "label", "value": ["a", "b"], "decoration": "excludes"}
	],
	"replicate_deletion": false,
	"deletion": false,
	"override": true,
	"enabled": true,
	"creation_time": "2026-01-02T03:04:05.000Z",
	"update_time": "2026-01-02T03:04:06.000Z",
	"speed": 0,
	"copy_by_chunk": false,
	"single_active_replication": true
}`

func policyNamed(id int, name string) string {
	p := strings.Replace(nginxPolicy, `"id": 7`, fmt.Sprintf(`"id": %d`, id), 1)
	return strings.Replace(p, "k8s.proj.team-a.nginx", name, 1)
}

var wantNginxPolicy = harbor.ReplicationPolicy{
	ID:                        7,
	Name:                      "k8s.proj.team-a.nginx",
	Description:               `{"managedBy":"harbor-apiserver"}`,
	SrcRegistry:               &harbor.Registry{ID: 3, Name: "dockerhub"},
	DestRegistry:              &harbor.Registry{ID: 0, Name: "Local"},
	DestNamespace:             "proj/k8s/team-a/nginx",
	DestNamespaceReplaceCount: new(int8),
	Trigger:                   &harbor.ReplicationTrigger{Type: "scheduled", Settings: &harbor.ReplicationTriggerSettings{Cron: "0 30 2 * * *"}},
	Filters: []harbor.ReplicationFilter{
		{Type: "name", Value: "library/nginx"},
		{Type: "tag", Value: "1.27", Decoration: "matches"},
		{Type: "label", Value: []any{"a", "b"}, Decoration: "excludes"},
	},
	Override:                true,
	Enabled:                 true,
	SingleActiveReplication: true,
	CreationTime:            time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
}

func TestListRegistries(t *testing.T) {
	c, got := replier(t, http.StatusOK, "", `[`+dockerHub+`]`)

	registries, err := c.ListRegistries(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(onlyGet("/api/v2.0/registries?page=1&page_size=100&sort=id"), got()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]harbor.Registry{{ID: 3, Name: "dockerhub"}}, registries); diff != "" {
		t.Errorf("registries (-want +got):\n%s", diff)
	}
}

func TestListReplicationPolicies(t *testing.T) {
	// Harbor matches names that contain the value, ignoring case.
	c, got := replier(t, http.StatusOK, "", `[`+strings.Join([]string{
		nginxPolicy,
		policyNamed(8, "other.k8s.proj.team-a.nginx"),
		policyNamed(9, "K8S.PROJ.team-a.nginx"),
	}, ",")+`]`)

	policies, err := c.ListReplicationPolicies(context.Background(), "k8s.proj.")
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(onlyGet("/api/v2.0/replication/policies?page=1&page_size=100&q=name%3D~k8s.proj.&sort=id"), got()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]harbor.ReplicationPolicy{wantNginxPolicy}, policies); diff != "" {
		t.Errorf("policies (-want +got):\n%s", diff)
	}
}

func TestListReplicationPoliciesEscapesTheNameForHarbor(t *testing.T) {
	c, got := replier(t, http.StatusOK, "", `[]`)
	if _, err := c.ListReplicationPolicies(context.Background(), "a+b%c"); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got()[0].URI)
	if err != nil {
		t.Fatal(err)
	}
	// Harbor unescapes q again after parsing the query.
	q, err := url.QueryUnescape(u.Query().Get("q"))
	if want := "name=~a+b%c"; q != want || err != nil {
		t.Errorf("harbor would read q=%q, %v; want %q", q, err, want)
	}
}

func TestListReplicationPoliciesError(t *testing.T) {
	c, _ := replier(t, http.StatusForbidden, "", "")
	policies, err := c.ListReplicationPolicies(context.Background(), "k8s.proj.")
	if !errors.Is(err, harbor.ErrForbidden) || policies != nil {
		t.Errorf("got %v, %v", policies, err)
	}
}

func TestGetReplicationPolicy(t *testing.T) {
	c, got := replier(t, http.StatusOK, "", nginxPolicy)

	p, err := c.GetReplicationPolicy(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(onlyGet("/api/v2.0/replication/policies/7"), got()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(&wantNginxPolicy, p); diff != "" {
		t.Errorf("policy (-want +got):\n%s", diff)
	}
}

func TestGetReplicationPolicyNotFound(t *testing.T) {
	c, _ := replier(t, http.StatusNotFound, "", `{"errors":[{"code":"NOT_FOUND","message":"policy 7 not found"}]}`)
	if p, err := c.GetReplicationPolicy(context.Background(), 7); !errors.Is(err, harbor.ErrNotFound) || p != nil {
		t.Errorf("got %v, %v", p, err)
	}
}

func TestCreateReplicationPolicy(t *testing.T) {
	c, got := replier(t, http.StatusCreated, "/api/v2.0/replication/policies/42", "")

	id, err := c.CreateReplicationPolicy(context.Background(), &harbor.ReplicationPolicy{
		Name:                      "k8s.proj.team-a.nginx",
		Description:               "{}",
		SrcRegistry:               &harbor.Registry{ID: 3},
		DestNamespace:             "proj/k8s/team-a/nginx",
		DestNamespaceReplaceCount: new(int8),
		Trigger:                   &harbor.ReplicationTrigger{Type: "manual"},
		Filters: []harbor.ReplicationFilter{
			{Type: "name", Value: "library/nginx"},
			{Type: "tag", Value: "1.27", Decoration: "matches"},
		},
		Override:                true,
		Enabled:                 true,
		SingleActiveReplication: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if id != 42 {
		t.Errorf("got ID %d, want 42", id)
	}
	want := []received{{
		Method:      http.MethodPost,
		URI:         "/api/v2.0/replication/policies",
		ContentType: "application/json",
		Body: decode(t, `{
			"name": "k8s.proj.team-a.nginx",
			"description": "{}",
			"src_registry": {"id": 3},
			"dest_namespace": "proj/k8s/team-a/nginx",
			"dest_namespace_replace_count": 0,
			"trigger": {"type": "manual"},
			"filters": [
				{"type": "name", "value": "library/nginx"},
				{"type": "tag", "value": "1.27", "decoration": "matches"}
			],
			"deletion": false,
			"override": true,
			"enabled": true,
			"single_active_replication": true
		}`),
	}}
	if diff := cmp.Diff(want, got()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

func TestCreateReplicationPolicyConflict(t *testing.T) {
	c, _ := replier(t, http.StatusConflict, "", `{"errors":[{"code":"CONFLICT","message":"policy exists"}]}`)
	_, err := c.CreateReplicationPolicy(context.Background(), &harbor.ReplicationPolicy{Name: "p"})
	if !errors.Is(err, harbor.ErrConflict) {
		t.Errorf("got %v, want %v", err, harbor.ErrConflict)
	}
	if want := "POST /api/v2.0/replication/policies: 409 Conflict: policy exists"; err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("error %q, want suffix %q", err, want)
	}
}

func TestCreatedIDNeedsCreatedWithLocation(t *testing.T) {
	for name, tc := range map[string]struct {
		status   int
		location string
	}{
		"200":                    {http.StatusOK, "/api/v2.0/replication/policies/42"},
		"no Location":            {http.StatusCreated, ""},
		"Location without an ID": {http.StatusCreated, "/api/v2.0/replication/policies/"},
		"non-numeric ID":         {http.StatusCreated, "/api/v2.0/replication/policies/abc"},
		"ID 0":                   {http.StatusCreated, "/api/v2.0/replication/policies/0"},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := replier(t, tc.status, tc.location, "")
			if id, err := c.CreateReplicationPolicy(context.Background(), &harbor.ReplicationPolicy{Name: "p"}); err == nil {
				t.Errorf("create returned ID %d", id)
			}
			if id, err := c.StartReplication(context.Background(), 7); err == nil {
				t.Errorf("start returned ID %d", id)
			}
		})
	}
}

func TestDeleteReplicationPolicy(t *testing.T) {
	c, got := replier(t, http.StatusOK, "", "")
	if err := c.DeleteReplicationPolicy(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	want := []received{{Method: http.MethodDelete, URI: "/api/v2.0/replication/policies/7"}}
	if diff := cmp.Diff(want, got()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

func TestDeleteRunningReplicationPolicy(t *testing.T) {
	c, _ := replier(t, http.StatusPreconditionFailed, "", `{"errors":[{"code":"PRECONDITION","message":"the execution 99 isn't in final status"}]}`)
	err := c.DeleteReplicationPolicy(context.Background(), 7)
	if !errors.Is(err, harbor.ErrPrecondition) {
		t.Errorf("got %v, want %v", err, harbor.ErrPrecondition)
	}
	if want := "DELETE /api/v2.0/replication/policies/7: 412 Precondition Failed: the execution 99 isn't in final status"; err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("error %q, want suffix %q", err, want)
	}
}

func TestStartReplication(t *testing.T) {
	c, got := replier(t, http.StatusCreated, "/api/v2.0/replication/executions/99", "")

	id, err := c.StartReplication(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}

	if id != 99 {
		t.Errorf("got ID %d, want 99", id)
	}
	want := []received{{
		Method:      http.MethodPost,
		URI:         "/api/v2.0/replication/executions",
		ContentType: "application/json",
		Body:        decode(t, `{"policy_id": 7}`),
	}}
	if diff := cmp.Diff(want, got()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

func TestLatestReplicationExecution(t *testing.T) {
	c, got := replier(t, http.StatusOK, "", `[{
		"id": 99,
		"policy_id": 7,
		"status": "InProgress",
		"status_text": "",
		"trigger": "scheduled",
		"start_time": "2026-01-02T03:04:05.000Z",
		"end_time": "0001-01-01T00:00:00.000Z",
		"total": 5,
		"failed": 1,
		"succeed": 2,
		"in_progress": 1,
		"stopped": 1
	}]`)

	e, err := c.LatestReplicationExecution(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(onlyGet("/api/v2.0/replication/executions?page=1&page_size=10&policy_id=7&sort=-id"), got()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
	want := &harbor.ReplicationExecution{
		ID:         99,
		PolicyID:   7,
		Status:     "InProgress",
		Trigger:    "scheduled",
		StartTime:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Total:      5,
		Failed:     1,
		Succeed:    2,
		InProgress: 1,
		Stopped:    1,
	}
	if diff := cmp.Diff(want, e); diff != "" {
		t.Errorf("execution (-want +got):\n%s", diff)
	}
}

func TestLatestReplicationExecutionOfANeverRunPolicy(t *testing.T) {
	c, _ := replier(t, http.StatusOK, "", `[]`)
	if e, err := c.LatestReplicationExecution(context.Background(), 7); e != nil || err != nil {
		t.Errorf("got %v, %v; want nil, nil", e, err)
	}
}

// executionPages serves executions, newest first, in the pages that the request asks for.
func executionPages(t *testing.T, executions []harbor.ReplicationExecution) *fakeHarbor {
	t.Helper()
	return newFakeHarbor(t, func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
		writeJSON(t, w, executions[min(len(executions), (page-1)*size):min(len(executions), page*size)])
	})
}

// skipped is how Harbor 2.14 records a run that it skipped because the previous run was still going.
func skipped(id int64) harbor.ReplicationExecution {
	return harbor.ReplicationExecution{ID: id, PolicyID: 7, Status: "Failed", StatusText: "Execution skipped: active replication still in progress.", Trigger: "scheduled"}
}

func TestLatestReplicationExecutionIgnoresSkippedRuns(t *testing.T) {
	running := harbor.ReplicationExecution{ID: 100, PolicyID: 7, Status: "InProgress", Trigger: "manual"}
	executions := []harbor.ReplicationExecution{}
	for id := int64(112); id > running.ID; id-- {
		executions = append(executions, skipped(id))
	}
	f := executionPages(t, append(executions, running, harbor.ReplicationExecution{ID: 99, PolicyID: 7, Status: "Succeed"}))

	e, err := newClient(t, f).LatestReplicationExecution(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"/api/v2.0/replication/executions?page=1&page_size=10&policy_id=7&sort=-id",
		"/api/v2.0/replication/executions?page=2&page_size=10&policy_id=7&sort=-id",
	}
	if diff := cmp.Diff(want, f.requests); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(&running, e); diff != "" {
		t.Errorf("execution (-want +got):\n%s", diff)
	}
}

func TestLatestReplicationExecutionOfOnlySkippedRuns(t *testing.T) {
	f := executionPages(t, []harbor.ReplicationExecution{skipped(2), skipped(1)})
	if e, err := newClient(t, f).LatestReplicationExecution(context.Background(), 7); e != nil || err != nil {
		t.Errorf("got %v, %v; want nil, nil", e, err)
	}
}

func TestLatestReplicationExecutionOfAnotherPolicy(t *testing.T) {
	c, _ := replier(t, http.StatusOK, "", `[{"id": 99, "policy_id": 8, "status": "InProgress"}]`)
	if e, err := c.LatestReplicationExecution(context.Background(), 7); err == nil {
		t.Errorf("returned %+v for policy 7", e)
	}
}

func TestListRunningReplicationExecutions(t *testing.T) {
	c, got := replier(t, http.StatusOK, "", `[
		{"id": 99, "policy_id": 7, "status": "InProgress"},
		{"id": 100, "policy_id": 8, "status": "InProgress"},
		{"id": 101, "policy_id": 7, "status": "Stopped"}
	]`)

	executions, err := c.ListRunningReplicationExecutions(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(onlyGet("/api/v2.0/replication/executions?page=1&page_size=100&policy_id=7&sort=id&status=InProgress"), got()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]harbor.ReplicationExecution{{ID: 99, PolicyID: 7, Status: "InProgress"}}, executions); diff != "" {
		t.Errorf("executions (-want +got):\n%s", diff)
	}
}

func TestStopReplicationExecution(t *testing.T) {
	// Harbor's handler returns no responder, so go-openapi writes 200 with null.
	c, got := replier(t, http.StatusOK, "", "null")
	if err := c.StopReplicationExecution(context.Background(), 99); err != nil {
		t.Fatal(err)
	}
	want := []received{{Method: http.MethodPut, URI: "/api/v2.0/replication/executions/99"}}
	if diff := cmp.Diff(want, got()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

func TestCreateReplicationPolicyThatCannotBeEncoded(t *testing.T) {
	c, got := replier(t, http.StatusCreated, "/api/v2.0/replication/policies/42", "")
	p := &harbor.ReplicationPolicy{Filters: []harbor.ReplicationFilter{{Type: "name", Value: func() {}}}}
	if id, err := c.CreateReplicationPolicy(context.Background(), p); err == nil {
		t.Errorf("created policy %d", id)
	}
	if len(got()) != 0 {
		t.Errorf("sent %v", got())
	}
}
