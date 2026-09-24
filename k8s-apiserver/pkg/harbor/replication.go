package harbor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Registry is a registry endpoint. ID 0 is the local Harbor.
type Registry struct {
	ID   int64  `json:"id"`
	Name string `json:"name,omitempty"`
}

// ReplicationPolicy copies artifacts between registries.
// Harbor fills in the name of both registries when it returns a policy.
type ReplicationPolicy struct {
	ID            int64     `json:"id,omitempty"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	SrcRegistry   *Registry `json:"src_registry,omitempty"`
	DestRegistry  *Registry `json:"dest_registry,omitempty"`
	DestNamespace string    `json:"dest_namespace"`
	// DestNamespaceReplaceCount is how many leading path segments DestNamespace replaces.
	// Harbor uses -1, its legacy mode, when this is nil.
	DestNamespaceReplaceCount *int8               `json:"dest_namespace_replace_count,omitempty"`
	Trigger                   *ReplicationTrigger `json:"trigger,omitempty"`
	Filters                   []ReplicationFilter `json:"filters"`
	Deletion                  bool                `json:"deletion"`
	Override                  bool                `json:"override"`
	Enabled                   bool                `json:"enabled"`
	SingleActiveReplication   bool                `json:"single_active_replication"`
	CreationTime              time.Time           `json:"creation_time,omitzero"`
}

type ReplicationTrigger struct {
	Type     string                      `json:"type"`
	Settings *ReplicationTriggerSettings `json:"trigger_settings,omitempty"`
}

type ReplicationTriggerSettings struct {
	Cron string `json:"cron"`
}

type ReplicationFilter struct {
	Type string `json:"type"`
	// Value is a string, or a list of strings for a label filter.
	Value      any    `json:"value"`
	Decoration string `json:"decoration,omitempty"`
}

type ReplicationExecution struct {
	ID         int64     `json:"id"`
	PolicyID   int64     `json:"policy_id"`
	Status     string    `json:"status"`
	StatusText string    `json:"status_text"`
	Trigger    string    `json:"trigger"`
	StartTime  time.Time `json:"start_time"`
	EndTime    time.Time `json:"end_time"`
	Total      int64     `json:"total"`
	Failed     int64     `json:"failed"`
	Succeed    int64     `json:"succeed"`
	InProgress int64     `json:"in_progress"`
	Stopped    int64     `json:"stopped"`
}

func (c *Client) ListRegistries(ctx context.Context) ([]Registry, error) {
	return list(ctx, c, "/registries", url.Values{"sort": {"id"}}, func(r Registry) int64 { return r.ID })
}

// ListReplicationPolicies lists the policies whose names start with namePrefix.
func (c *Client) ListReplicationPolicies(ctx context.Context, namePrefix string) ([]ReplicationPolicy, error) {
	// Harbor unescapes q a second time, and matches names that contain the value, ignoring case.
	query := url.Values{"sort": {"id"}, "q": {"name=~" + url.QueryEscape(namePrefix)}}
	policies, err := list(ctx, c, "/replication/policies", query, func(p ReplicationPolicy) int64 { return p.ID })
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(policies, func(p ReplicationPolicy) bool { return !strings.HasPrefix(p.Name, namePrefix) }), nil
}

func (c *Client) GetReplicationPolicy(ctx context.Context, id int64) (*ReplicationPolicy, error) {
	var p ReplicationPolicy
	if _, err := c.do(ctx, http.MethodGet, policyPath(id), nil, nil, http.StatusOK, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// CreateReplicationPolicy returns the new policy's ID. A policy with the same name is ErrConflict.
func (c *Client) CreateReplicationPolicy(ctx context.Context, p *ReplicationPolicy) (int64, error) {
	header, err := c.do(ctx, http.MethodPost, "/replication/policies", nil, p, http.StatusCreated, nil)
	if err != nil {
		return 0, err
	}
	return createdID(header)
}

// DeleteReplicationPolicy returns ErrPrecondition while an execution of the policy is running.
func (c *Client) DeleteReplicationPolicy(ctx context.Context, id int64) error {
	_, err := c.do(ctx, http.MethodDelete, policyPath(id), nil, nil, http.StatusOK, nil)
	return err
}

// StartReplication starts an execution of a policy, and returns the execution's ID.
func (c *Client) StartReplication(ctx context.Context, policyID int64) (int64, error) {
	body := map[string]int64{"policy_id": policyID}
	header, err := c.do(ctx, http.MethodPost, "/replication/executions", nil, body, http.StatusCreated, nil)
	if err != nil {
		return 0, err
	}
	return createdID(header)
}

// executionPageSize is how many executions LatestReplicationExecution reads at once.
// A run as long as 10 schedule intervals needs a second page.
const executionPageSize = 10

// LatestReplicationExecution returns the policy's newest execution that Harbor did not skip, or nil if there is none.
// Harbor 2.14 records a run that it skips, because the previous run is still going, as a newer failed execution.
func (c *Client) LatestReplicationExecution(ctx context.Context, policyID int64) (*ReplicationExecution, error) {
	for page := 1; page <= maxPages; page++ {
		query := url.Values{"policy_id": {strconv.FormatInt(policyID, 10)}, "sort": {"-id"}, "page": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(executionPageSize)}}
		var executions []ReplicationExecution
		if _, err := c.do(ctx, http.MethodGet, "/replication/executions", query, nil, http.StatusOK, &executions); err != nil {
			return nil, err
		}
		for i, e := range executions {
			if e.PolicyID != policyID {
				return nil, fmt.Errorf("harbor returned execution %d of policy %d, not of policy %d", e.ID, e.PolicyID, policyID)
			}
			if !strings.HasPrefix(e.StatusText, "Execution skipped") {
				return &executions[i], nil
			}
		}
		if len(executions) < executionPageSize {
			return nil, nil
		}
	}
	return nil, fmt.Errorf("GET /replication/executions: more than %d pages of skipped executions", maxPages)
}

// ListRunningReplicationExecutions lists a policy's executions whose status is InProgress.
// It drops any other execution, so a Harbor that ignores the filters cannot make a caller stop another policy.
func (c *Client) ListRunningReplicationExecutions(ctx context.Context, policyID int64) ([]ReplicationExecution, error) {
	query := url.Values{"policy_id": {strconv.FormatInt(policyID, 10)}, "status": {"InProgress"}, "sort": {"id"}}
	executions, err := list(ctx, c, "/replication/executions", query, func(e ReplicationExecution) int64 { return e.ID })
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(executions, func(e ReplicationExecution) bool { return e.PolicyID != policyID || e.Status != "InProgress" }), nil
}

func (c *Client) StopReplicationExecution(ctx context.Context, id int64) error {
	_, err := c.do(ctx, http.MethodPut, "/replication/executions/"+strconv.FormatInt(id, 10), nil, nil, http.StatusOK, nil)
	return err
}

func policyPath(id int64) string {
	return "/replication/policies/" + strconv.FormatInt(id, 10)
}

// createdID parses the ID that ends a Location header, such as /api/v2.0/replication/policies/7.
func createdID(header http.Header) (int64, error) {
	location := header.Get("Location")
	id, err := strconv.ParseInt(location[strings.LastIndex(location, "/")+1:], 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("harbor returned Location %q without an ID", location)
	}
	return id, nil
}
