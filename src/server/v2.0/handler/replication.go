// Copyright Project Harbor Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package handler

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-openapi/runtime/middleware"
	"github.com/go-openapi/strfmt"

	"github.com/goharbor/harbor/src/common/rbac"
	"github.com/goharbor/harbor/src/controller/replication"
	repctlmodel "github.com/goharbor/harbor/src/controller/replication/model"
	"github.com/goharbor/harbor/src/jobservice/job"
	"github.com/goharbor/harbor/src/lib/errors"
	"github.com/goharbor/harbor/src/lib/q"
	"github.com/goharbor/harbor/src/pkg/reg/model"
	"github.com/goharbor/harbor/src/pkg/task"
	"github.com/goharbor/harbor/src/server/v2.0/models"
	operation "github.com/goharbor/harbor/src/server/v2.0/restapi/operations/replication"
)

func newReplicationAPI() *replicationAPI {
	return &replicationAPI{
		ctl: replication.Ctl,
	}
}

type replicationAPI struct {
	BaseAPI
	ctl replication.Controller
}

func (r *replicationAPI) Prepare(_ context.Context, _ string, _ any) middleware.Responder {
	return nil
}

// requireAccess passes callers with the system permission. Otherwise, it passes callers with the
// permission on the project that the policy pulls into.
func (r *replicationAPI) requireAccess(ctx context.Context, action rbac.Action, resource rbac.Resource, policy *repctlmodel.Policy) error {
	err := r.RequireSystemAccess(ctx, action, resource)
	if !errors.IsErr(err, errors.ForbiddenCode) {
		return err
	}
	project, ok := pullProject(policy)
	if !ok {
		return err
	}
	has, permErr := r.HasProjectPermission(ctx, project, action, resource)
	if permErr != nil {
		return permErr
	}
	if !has {
		return err
	}
	return nil
}

// requirePolicyAccess is requireAccess for a stored policy.
// It gets the policy only for callers without the system permission.
func (r *replicationAPI) requirePolicyAccess(ctx context.Context, id int64, action rbac.Action, resource rbac.Resource) error {
	if err := r.RequireSystemAccess(ctx, action, resource); !errors.IsErr(err, errors.ForbiddenCode) {
		return err
	}
	policy, err := r.ctl.GetPolicy(ctx, id)
	if err != nil {
		return err
	}
	return r.requireAccess(ctx, action, resource, policy)
}

// requireExecutionAccess is requirePolicyAccess for the policy of an execution.
func (r *replicationAPI) requireExecutionAccess(ctx context.Context, id int64, action rbac.Action) error {
	if err := r.RequireSystemAccess(ctx, action, rbac.ResourceReplication); !errors.IsErr(err, errors.ForbiddenCode) {
		return err
	}
	execution, err := r.ctl.GetExecution(ctx, id)
	if err != nil {
		return err
	}
	return r.requirePolicyAccess(ctx, execution.PolicyID, action, rbac.ResourceReplication)
}

// pullProject returns the project that the policy pulls into from a remote registry.
func pullProject(policy *repctlmodel.Policy) (string, bool) {
	pulls := policy.SrcRegistry != nil && policy.SrcRegistry.ID != 0 &&
		(policy.DestRegistry == nil || policy.DestRegistry.ID == 0)
	if !pulls || policy.DestNamespace == "" {
		return "", false
	}
	return strings.Split(policy.DestNamespace, "/")[0], true
}

func (r *replicationAPI) CreateReplicationPolicy(ctx context.Context, params operation.CreateReplicationPolicyParams) middleware.Responder {
	sc, err := r.GetSecurityContext(ctx)
	if err != nil {
		return r.SendError(ctx, err)
	}
	policy := &repctlmodel.Policy{
		Name:              params.Policy.Name,
		Description:       params.Policy.Description,
		Creator:           sc.GetUsername(),
		DestNamespace:     params.Policy.DestNamespace,
		ReplicateDeletion: params.Policy.Deletion,
		Override:          params.Policy.Override,
		Enabled:           params.Policy.Enabled,
	}
	// Make this field be optional to keep backward compatibility
	if params.Policy.DestNamespaceReplaceCount != nil {
		policy.DestNamespaceReplaceCount = *params.Policy.DestNamespaceReplaceCount
	} else {
		policy.DestNamespaceReplaceCount = -1 // -1 mean the legacy mode
	}
	if params.Policy.SrcRegistry != nil {
		policy.SrcRegistry = &model.Registry{
			ID: params.Policy.SrcRegistry.ID,
		}
	}
	if params.Policy.DestRegistry != nil {
		policy.DestRegistry = &model.Registry{
			ID: params.Policy.DestRegistry.ID,
		}
	}
	if len(params.Policy.Filters) > 0 {
		for _, filter := range params.Policy.Filters {
			policy.Filters = append(policy.Filters, &model.Filter{
				Type:       filter.Type,
				Value:      filter.Value,
				Decoration: filter.Decoration,
			})
		}
	}
	if params.Policy.Trigger != nil {
		policy.Trigger = &model.Trigger{
			Type: params.Policy.Trigger.Type,
		}
		if params.Policy.Trigger.TriggerSettings != nil {
			policy.Trigger.Settings = &model.TriggerSettings{
				Cron: params.Policy.Trigger.TriggerSettings.Cron,
			}
		}
	}
	if params.Policy.Speed != nil {
		if *params.Policy.Speed < 0 {
			*params.Policy.Speed = 0
		}
		policy.Speed = *params.Policy.Speed
	}

	if params.Policy.CopyByChunk != nil {
		policy.CopyByChunk = *params.Policy.CopyByChunk
	}

	if err := r.requireAccess(ctx, rbac.ActionCreate, rbac.ResourceReplicationPolicy, policy); err != nil {
		return r.SendError(ctx, err)
	}

	if params.Policy.SingleActiveReplication != nil {
		// Validate and assign SingleActiveReplication only for non-event_based triggers
		if params.Policy.Trigger != nil && params.Policy.Trigger.Type == model.TriggerTypeEventBased && *params.Policy.SingleActiveReplication {
			return r.SendError(ctx, fmt.Errorf("single active replication is not allowed for event_based triggers"))
		}
		policy.SingleActiveReplication = *params.Policy.SingleActiveReplication
	}

	id, err := r.ctl.CreatePolicy(ctx, policy)
	if err != nil {
		return r.SendError(ctx, err)
	}
	location := fmt.Sprintf("%s/%d", strings.TrimSuffix(params.HTTPRequest.URL.Path, "/"), id)
	return operation.NewCreateReplicationPolicyCreated().WithLocation(location)
}

func (r *replicationAPI) UpdateReplicationPolicy(ctx context.Context, params operation.UpdateReplicationPolicyParams) middleware.Responder {
	if err := r.requirePolicyAccess(ctx, params.ID, rbac.ActionUpdate, rbac.ResourceReplicationPolicy); err != nil {
		return r.SendError(ctx, err)
	}
	policy := &repctlmodel.Policy{
		ID:                params.ID,
		Name:              params.Policy.Name,
		Description:       params.Policy.Description,
		DestNamespace:     params.Policy.DestNamespace,
		ReplicateDeletion: params.Policy.Deletion,
		Override:          params.Policy.Override,
		Enabled:           params.Policy.Enabled,
	}
	// Make this field be optional to keep backward compatibility
	if params.Policy.DestNamespaceReplaceCount != nil {
		policy.DestNamespaceReplaceCount = *params.Policy.DestNamespaceReplaceCount
	} else {
		policy.DestNamespaceReplaceCount = -1 // -1 mean the legacy mode
	}

	if params.Policy.SrcRegistry != nil {
		policy.SrcRegistry = &model.Registry{
			ID: params.Policy.SrcRegistry.ID,
		}
	}
	if params.Policy.DestRegistry != nil {
		policy.DestRegistry = &model.Registry{
			ID: params.Policy.DestRegistry.ID,
		}
	}
	if len(params.Policy.Filters) > 0 {
		for _, filter := range params.Policy.Filters {
			policy.Filters = append(policy.Filters, &model.Filter{
				Type:       filter.Type,
				Value:      filter.Value,
				Decoration: filter.Decoration,
			})
		}
	}
	if params.Policy.Trigger != nil {
		policy.Trigger = &model.Trigger{
			Type: params.Policy.Trigger.Type,
		}
		if params.Policy.Trigger.TriggerSettings != nil {
			policy.Trigger.Settings = &model.TriggerSettings{
				Cron: params.Policy.Trigger.TriggerSettings.Cron,
			}
		}
	}
	if params.Policy.Speed != nil {
		if *params.Policy.Speed < 0 {
			*params.Policy.Speed = 0
		}
		policy.Speed = *params.Policy.Speed
	}

	if params.Policy.CopyByChunk != nil {
		policy.CopyByChunk = *params.Policy.CopyByChunk
	}

	if err := r.requireAccess(ctx, rbac.ActionUpdate, rbac.ResourceReplicationPolicy, policy); err != nil {
		return r.SendError(ctx, err)
	}

	if params.Policy.SingleActiveReplication != nil {
		// Validate and assign SingleActiveReplication only for non-event_based triggers
		if params.Policy.Trigger != nil && params.Policy.Trigger.Type == model.TriggerTypeEventBased && *params.Policy.SingleActiveReplication {
			return r.SendError(ctx, fmt.Errorf("single active replication is not allowed for event_based triggers"))
		}
		policy.SingleActiveReplication = *params.Policy.SingleActiveReplication
	}

	if err := r.ctl.UpdatePolicy(ctx, policy); err != nil {
		return r.SendError(ctx, err)
	}
	return operation.NewUpdateReplicationPolicyOK()
}

func (r *replicationAPI) ListReplicationPolicies(ctx context.Context, params operation.ListReplicationPoliciesParams) middleware.Responder {
	systemErr := r.RequireSystemAccess(ctx, rbac.ActionList, rbac.ResourceReplicationPolicy)
	if systemErr != nil && !errors.IsErr(systemErr, errors.ForbiddenCode) {
		return r.SendError(ctx, systemErr)
	}
	query, err := r.BuildQuery(ctx, params.Q, params.Sort, params.Page, params.PageSize)
	if err != nil {
		return r.SendError(ctx, err)
	}
	if params.Name != nil {
		query.Keywords["Name"] = &q.FuzzyMatchValue{
			Value: *params.Name,
		}
	}
	var total int64
	var policies []*repctlmodel.Policy
	if systemErr == nil {
		total, err = r.ctl.PolicyCount(ctx, query)
		if err != nil {
			return r.SendError(ctx, err)
		}
		policies, err = r.ctl.ListPolicies(ctx, query)
	} else {
		policies, err = r.listProjectPolicies(ctx, query)
		total = int64(len(policies))
		policies = page(policies, query.PageNumber, query.PageSize)
	}
	if err != nil {
		return r.SendError(ctx, err)
	}
	var result []*models.ReplicationPolicy
	for _, policy := range policies {
		result = append(result, convertReplicationPolicy(policy))
	}
	return operation.NewListReplicationPoliciesOK().
		WithXTotalCount(total).
		WithLink(r.Links(ctx, params.HTTPRequest.URL, total, query.PageNumber, query.PageSize).String()).
		WithPayload(result)
}

// listProjectPolicies lists, on all pages, the policies that pull into projects where the caller can list policies.
func (r *replicationAPI) listProjectPolicies(ctx context.Context, query *q.Query) ([]*repctlmodel.Policy, error) {
	allPages := q.MustClone(query)
	allPages.PageNumber, allPages.PageSize = 0, 0
	allPages.Keywords["DestRegistryID"] = 0
	policies, err := r.ctl.ListPolicies(ctx, allPages)
	if err != nil {
		return nil, err
	}
	canList := map[string]bool{}
	var result []*repctlmodel.Policy
	for _, policy := range policies {
		project, ok := pullProject(policy)
		if !ok {
			continue
		}
		allowed, checked := canList[project]
		if !checked {
			allowed, err = r.HasProjectPermission(ctx, project, rbac.ActionList, rbac.ResourceReplicationPolicy)
			if err != nil {
				return nil, err
			}
			canList[project] = allowed
		}
		if allowed {
			result = append(result, policy)
		}
	}
	return result, nil
}

func page[T any](items []T, number, size int64) []T {
	if size <= 0 {
		return items
	}
	start := min(max(number-1, 0)*size, int64(len(items)))
	return items[start:min(start+size, int64(len(items)))]
}

func (r *replicationAPI) GetReplicationPolicy(ctx context.Context, params operation.GetReplicationPolicyParams) middleware.Responder {
	if err := r.requirePolicyAccess(ctx, params.ID, rbac.ActionRead, rbac.ResourceReplicationPolicy); err != nil {
		return r.SendError(ctx, err)
	}
	policy, err := r.ctl.GetPolicy(ctx, params.ID)
	if err != nil {
		return r.SendError(ctx, err)
	}
	return operation.NewGetReplicationPolicyOK().WithPayload(convertReplicationPolicy(policy))
}

func (r *replicationAPI) DeleteReplicationPolicy(ctx context.Context, params operation.DeleteReplicationPolicyParams) middleware.Responder {
	if err := r.requirePolicyAccess(ctx, params.ID, rbac.ActionDelete, rbac.ResourceReplicationPolicy); err != nil {
		return r.SendError(ctx, err)
	}
	if err := r.ctl.DeletePolicy(ctx, params.ID); err != nil {
		return r.SendError(ctx, err)
	}
	return operation.NewDeleteReplicationPolicyOK()
}

func (r *replicationAPI) StartReplication(ctx context.Context, params operation.StartReplicationParams) middleware.Responder {
	if err := r.requirePolicyAccess(ctx, params.Execution.PolicyID, rbac.ActionCreate, rbac.ResourceReplication); err != nil {
		return r.SendError(ctx, err)
	}
	policy, err := r.ctl.GetPolicy(ctx, params.Execution.PolicyID)
	if err != nil {
		return r.SendError(ctx, err)
	}
	// the legacy replication scheduler job("src/jobservice/job/impl/replication/scheduler.go") calls the start replication API
	// to trigger the scheduled replication, a query string "trigger" is added when sending the request
	// here is the logic to cover this part
	trigger := task.ExecutionTriggerManual
	if params.HTTPRequest.URL.Query().Get("trigger") == "scheduled" {
		trigger = task.ExecutionTriggerSchedule
	}

	executionID, err := r.ctl.Start(ctx, policy, nil, trigger)
	if err != nil {
		return r.SendError(ctx, err)
	}
	location := strings.TrimSuffix(params.HTTPRequest.URL.Path, "/") + "/" + strconv.FormatInt(executionID, 10)
	return operation.NewStartReplicationCreated().WithLocation(location)
}

func (r *replicationAPI) StopReplication(ctx context.Context, params operation.StopReplicationParams) middleware.Responder {
	if err := r.requireExecutionAccess(ctx, params.ID, rbac.ActionCreate); err != nil {
		return r.SendError(ctx, err)
	}
	if err := r.ctl.Stop(ctx, params.ID); err != nil {
		return r.SendError(ctx, err)
	}
	return nil
}

func (r *replicationAPI) ListReplicationExecutions(ctx context.Context, params operation.ListReplicationExecutionsParams) middleware.Responder {
	var err error
	if params.PolicyID != nil {
		err = r.requirePolicyAccess(ctx, *params.PolicyID, rbac.ActionList, rbac.ResourceReplication)
	} else {
		err = r.RequireSystemAccess(ctx, rbac.ActionList, rbac.ResourceReplication)
	}
	if errors.IsNotFoundErr(err) {
		// Only callers with the system permission see the executions of a deleted policy.
		return operation.NewListReplicationExecutionsOK().WithXTotalCount(0).WithPayload([]*models.ReplicationExecution{})
	}
	if err != nil {
		return r.SendError(ctx, err)
	}
	query, err := r.BuildQuery(ctx, nil, params.Sort, params.Page, params.PageSize)
	if err != nil {
		return r.SendError(ctx, err)
	}
	if params.PolicyID != nil {
		query.Keywords["PolicyID"] = *params.PolicyID
	}
	if params.Status != nil {
		status := *params.Status
		// as we convert the status when responding requests to keep the backward compatibility,
		// here we need to reverse-convert the status
		switch status {
		case "InProgress":
			status = job.RunningStatus.String()
		case "Succeed":
			status = job.SuccessStatus.String()
		case "Stopped":
			status = job.StoppedStatus.String()
		case "Failed":
			status = job.ErrorStatus.String()
		}
		query.Keywords["Status"] = status
	}
	if params.Trigger != nil {
		trigger := *params.Trigger
		// as we convert the trigger when responding requests to keep the backward compatibility,
		// here we need to reverse-convert the trigger
		switch trigger {
		case "manual":
			trigger = task.ExecutionTriggerManual
		case "scheduled":
			trigger = task.ExecutionTriggerSchedule
		case "event_based":
			trigger = task.ExecutionTriggerEvent
		}
		query.Keywords["Trigger"] = trigger
	}

	total, err := r.ctl.ExecutionCount(ctx, query)
	if err != nil {
		return r.SendError(ctx, err)
	}
	executions, err := r.ctl.ListExecutions(ctx, query)
	if err != nil {
		return r.SendError(ctx, err)
	}

	var execs []*models.ReplicationExecution
	for _, execution := range executions {
		execs = append(execs, convertExecution(execution))
	}

	return operation.NewListReplicationExecutionsOK().
		WithXTotalCount(total).
		WithLink(r.Links(ctx, params.HTTPRequest.URL, total, query.PageNumber, query.PageSize).String()).
		WithPayload(execs)
}

func (r *replicationAPI) GetReplicationExecution(ctx context.Context, params operation.GetReplicationExecutionParams) middleware.Responder {
	if err := r.requireExecutionAccess(ctx, params.ID, rbac.ActionRead); err != nil {
		return r.SendError(ctx, err)
	}
	execution, err := r.ctl.GetExecution(ctx, params.ID)
	if err != nil {
		return r.SendError(ctx, err)
	}
	return operation.NewGetReplicationExecutionOK().WithPayload(convertExecution(execution))
}

func (r *replicationAPI) ListReplicationTasks(ctx context.Context, params operation.ListReplicationTasksParams) middleware.Responder {
	if err := r.requireExecutionAccess(ctx, params.ID, rbac.ActionList); err != nil {
		return r.SendError(ctx, err)
	}
	// check the existence of the replication execution
	_, err := r.ctl.GetExecution(ctx, params.ID)
	if err != nil {
		return r.SendError(ctx, err)
	}

	query, err := r.BuildQuery(ctx, nil, params.Sort, params.Page, params.PageSize)
	if err != nil {
		return r.SendError(ctx, err)
	}
	query.Keywords["ExecutionID"] = params.ID
	if params.Status != nil {
		var status any = *params.Status
		// as we convert the status when responding requests to keep the backward compatibility,
		// here we need to reverse-convert the status
		// the status "pending" and "stopped" is same with jobservice, no need to convert
		switch status {
		case "InProgress":
			status = &q.OrList{
				Values: []any{
					job.ScheduledStatus.String(),
					job.RunningStatus.String(),
				},
			}
		case "Succeed":
			status = job.SuccessStatus.String()
		case "Failed":
			status = job.ErrorStatus.String()
		}
		query.Keywords["Status"] = status
	}
	if params.ResourceType != nil {
		query.Keywords["ExtraAttrs.resource_type"] = *params.ResourceType
	}

	total, err := r.ctl.TaskCount(ctx, query)
	if err != nil {
		return r.SendError(ctx, err)
	}

	tasks, err := r.ctl.ListTasks(ctx, query)
	if err != nil {
		return r.SendError(ctx, err)
	}

	var tks []*models.ReplicationTask
	for _, task := range tasks {
		tks = append(tks, convertTask(task))
	}

	return operation.NewListReplicationTasksOK().
		WithXTotalCount(total).
		WithLink(r.Links(ctx, params.HTTPRequest.URL, total, query.PageNumber, query.PageSize).String()).
		WithPayload(tks)
}

func (r *replicationAPI) GetReplicationLog(ctx context.Context, params operation.GetReplicationLogParams) middleware.Responder {
	if err := r.requireExecutionAccess(ctx, params.ID, rbac.ActionRead); err != nil {
		return r.SendError(ctx, err)
	}
	execution, err := r.ctl.GetExecution(ctx, params.ID)
	if err != nil {
		return r.SendError(ctx, err)
	}
	task, err := r.ctl.GetTask(ctx, params.TaskID)
	if err != nil {
		return r.SendError(ctx, err)
	}
	if execution.ID != task.ExecutionID {
		return r.SendError(ctx, errors.New(nil).
			WithCode(errors.NotFoundCode).
			WithMessagef("execution %d contains no task with ID %d", params.ID, params.TaskID))
	}
	log, err := r.ctl.GetTaskLog(ctx, params.TaskID)
	if err != nil {
		return r.SendError(ctx, err)
	}
	return operation.NewGetReplicationLogOK().WithContentType("text/plain").WithPayload(string(log))
}

func convertReplicationPolicy(policy *repctlmodel.Policy) *models.ReplicationPolicy {
	replaceCount := policy.DestNamespaceReplaceCount
	p := &models.ReplicationPolicy{
		CreationTime:              strfmt.DateTime(policy.CreationTime),
		Deletion:                  policy.ReplicateDeletion,
		Description:               policy.Description,
		DestNamespace:             policy.DestNamespace,
		DestNamespaceReplaceCount: &replaceCount,
		Enabled:                   policy.Enabled,
		ID:                        policy.ID,
		Name:                      policy.Name,
		Override:                  policy.Override,
		ReplicateDeletion:         policy.ReplicateDeletion,
		Speed:                     &policy.Speed,
		UpdateTime:                strfmt.DateTime(policy.UpdateTime),
		CopyByChunk:               &policy.CopyByChunk,
		SingleActiveReplication:   &policy.SingleActiveReplication,
	}
	if policy.SrcRegistry != nil {
		p.SrcRegistry = convertRegistry(policy.SrcRegistry)
	}
	if policy.DestRegistry != nil {
		p.DestRegistry = convertRegistry(policy.DestRegistry)
	}
	if len(policy.Filters) > 0 {
		for _, filter := range policy.Filters {
			p.Filters = append(p.Filters, &models.ReplicationFilter{
				Type:       string(filter.Type),
				Value:      filter.Value,
				Decoration: filter.Decoration,
			})
		}
	}
	if policy.Trigger != nil {
		trigger := &models.ReplicationTrigger{
			Type: string(policy.Trigger.Type),
		}
		if policy.Trigger.Settings != nil {
			trigger.TriggerSettings = &models.ReplicationTriggerSettings{
				Cron: policy.Trigger.Settings.Cron,
			}
		}
		p.Trigger = trigger
	}
	return p
}

func convertRegistry(registry *model.Registry) *models.Registry {
	r := &models.Registry{
		CreationTime: strfmt.DateTime(registry.CreationTime),
		Description:  registry.Description,
		ID:           registry.ID,
		Insecure:     registry.Insecure,
		Name:         registry.Name,
		Status:       registry.Status,
		Type:         string(registry.Type),
		UpdateTime:   strfmt.DateTime(registry.UpdateTime),
		URL:          registry.URL,
	}
	if registry.Credential != nil {
		credential := &models.RegistryCredential{
			AccessKey: registry.Credential.AccessKey,
			Type:      string(registry.Credential.Type),
		}
		if len(registry.Credential.AccessSecret) > 0 {
			credential.AccessSecret = "*****"
		}
		r.Credential = credential
	}
	if len(registry.CACertificate) > 0 {
		r.CaCertificate = &registry.CACertificate
	}
	return r
}

func convertExecution(execution *replication.Execution) *models.ReplicationExecution {
	exec := &models.ReplicationExecution{
		ID:         execution.ID,
		PolicyID:   execution.PolicyID,
		StatusText: execution.StatusMessage,
		StartTime:  strfmt.DateTime(execution.StartTime),
		EndTime:    strfmt.DateTime(execution.EndTime),
	}
	// keep backward compatibility
	if execution.Metrics != nil {
		exec.Total = execution.Metrics.TaskCount
		exec.Succeed = execution.Metrics.SuccessTaskCount
		exec.Failed = execution.Metrics.ErrorTaskCount
		exec.InProgress = execution.Metrics.PendingTaskCount +
			execution.Metrics.ScheduledTaskCount + execution.Metrics.RunningTaskCount
		exec.Stopped = execution.Metrics.StoppedTaskCount
	}
	switch execution.Trigger {
	case task.ExecutionTriggerManual:
		exec.Trigger = "manual"
	case task.ExecutionTriggerSchedule:
		exec.Trigger = "scheduled"
	case task.ExecutionTriggerEvent:
		exec.Trigger = "event_based"
	}
	switch execution.Status {
	case job.RunningStatus.String():
		exec.Status = "InProgress"
	case job.SuccessStatus.String():
		exec.Status = "Succeed"
	case job.StoppedStatus.String():
		exec.Status = "Stopped"
	case job.ErrorStatus.String():
		exec.Status = "Failed"
	}

	return exec
}

func convertTask(task *replication.Task) *models.ReplicationTask {
	tk := &models.ReplicationTask{
		ID:           task.ID,
		ExecutionID:  task.ExecutionID,
		JobID:        task.JobID,
		Operation:    task.Operation,
		ResourceType: task.ResourceType,
		SrcResource:  task.SourceResource,
		DstResource:  task.DestinationResource,
		StartTime:    strfmt.DateTime(task.StartTime),
		EndTime:      strfmt.DateTime(task.EndTime),
	}
	// keep backward compatibility
	switch task.Status {
	case job.ScheduledStatus.String(), job.RunningStatus.String():
		tk.Status = "InProgress"
	case job.SuccessStatus.String():
		tk.Status = "Succeed"
	case job.ErrorStatus.String():
		tk.Status = "Failed"
	// the status "pending" and "stopped" is same with jobservice, no need to convert
	default:
		tk.Status = task.Status
	}
	return tk
}
