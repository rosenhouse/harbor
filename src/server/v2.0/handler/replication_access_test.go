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
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	testifymock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"

	"github.com/goharbor/harbor/src/common/security"
	robotsec "github.com/goharbor/harbor/src/common/security/robot"
	"github.com/goharbor/harbor/src/controller/project"
	"github.com/goharbor/harbor/src/controller/replication"
	repctlmodel "github.com/goharbor/harbor/src/controller/replication/model"
	"github.com/goharbor/harbor/src/controller/robot"
	"github.com/goharbor/harbor/src/lib/errors"
	"github.com/goharbor/harbor/src/lib/q"
	"github.com/goharbor/harbor/src/pkg/permission/types"
	"github.com/goharbor/harbor/src/pkg/reg/model"
	robotmodel "github.com/goharbor/harbor/src/pkg/robot/model"
	"github.com/goharbor/harbor/src/server/v2.0/models"
	"github.com/goharbor/harbor/src/server/v2.0/restapi"
	replicationtesting "github.com/goharbor/harbor/src/testing/controller/replication"
	"github.com/goharbor/harbor/src/testing/mock"
	htesting "github.com/goharbor/harbor/src/testing/server/v2.0/handler"
)

type replicationAccessTestSuite struct {
	htesting.Suite
	ctl     *replicationtesting.Controller
	granted map[string]bool
}

func (s *replicationAccessTestSuite) SetupSuite() {
	s.ctl = &replicationtesting.Controller{}
	s.Config = &restapi.Config{ReplicationAPI: &replicationAPI{ctl: s.ctl}}
	s.Suite.SetupSuite()
}

func (s *replicationAccessTestSuite) TearDownSuite() {
	projectCtlMock.ExpectedCalls, projectCtlMock.Calls = nil, nil
	s.Suite.TearDownSuite()
}

func (s *replicationAccessTestSuite) SetupTest() {
	s.ctl.ExpectedCalls, s.ctl.Calls = nil, nil
	s.Security.ExpectedCalls, s.Security.Calls = nil, nil
	projectCtlMock.ExpectedCalls, projectCtlMock.Calls = nil, nil

	s.granted = map[string]bool{}
	s.Security.On("IsAuthenticated").Return(true)
	s.Security.On("GetUsername").Return("robot$replicator")
	s.Security.On("Can", mock.Anything, mock.Anything, mock.Anything).Return(
		func(_ context.Context, action types.Action, resource types.Resource) bool {
			return s.granted[resource.String()+":"+action.String()]
		})

	projectCtlMock.On("GetByName", mock.Anything, "p").Return(&project.Project{ProjectID: 7, Name: "p"}, nil)
	projectCtlMock.On("GetByName", mock.Anything, "other").Return(&project.Project{ProjectID: 8, Name: "other"}, nil)
	projectCtlMock.On("GetByName", mock.Anything, "broken").Return(nil, errors.New("database is down"))
	projectCtlMock.On("GetByName", mock.Anything, mock.Anything).Return(nil, errors.NotFoundError(nil))

	s.ctl.On("DeletePolicy", mock.Anything, mock.Anything).Return(nil)
	s.ctl.On("UpdatePolicy", mock.Anything, mock.Anything).Return(nil)
	s.ctl.On("Start", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(int64(1), nil)
	s.ctl.On("ExecutionCount", mock.Anything, mock.Anything).Return(int64(0), nil)
	s.ctl.On("ListExecutions", mock.Anything, mock.Anything).Return(nil, nil)
	s.ctl.On("Stop", mock.Anything, mock.Anything).Return(nil)
	s.ctl.On("TaskCount", mock.Anything, mock.Anything).Return(int64(0), nil)
	s.ctl.On("ListTasks", mock.Anything, mock.Anything).Return(nil, nil)
	s.ctl.On("GetTask", mock.Anything, int64(1)).Return(&replication.Task{ID: 1, ExecutionID: 50}, nil)
	s.ctl.On("GetTaskLog", mock.Anything, int64(1)).Return([]byte("log"), nil)
}

// grant lets the caller do each action on the resource, such as "/project/7/replication-policy".
func (s *replicationAccessTestSuite) grant(resource string, actions ...string) {
	for _, action := range actions {
		s.granted[resource+":"+action] = true
	}
}

func pullPolicy(destNamespace string) *models.ReplicationPolicy {
	return &models.ReplicationPolicy{Name: "pull", SrcRegistry: &models.Registry{ID: 3}, DestNamespace: destNamespace}
}

func (s *replicationAccessTestSuite) TestCreateWithProjectPermission() {
	s.grant("/project/7/replication-policy", "create")
	s.ctl.On("CreatePolicy", mock.Anything, mock.Anything).Return(int64(1), nil)

	res, err := s.PostJSON("/replication/policies", pullPolicy("p/team/app"))
	s.Require().NoError(err)
	s.Equal(http.StatusCreated, res.StatusCode)
	s.True(s.called("CreatePolicy"))
}

func (s *replicationAccessTestSuite) TestCreateWithSystemPermission() {
	s.grant("/system/replication-policy", "create")
	s.ctl.On("CreatePolicy", mock.Anything, mock.Anything).Return(int64(1), nil)

	push := &models.ReplicationPolicy{Name: "push", DestRegistry: &models.Registry{ID: 3}}
	res, err := s.PostJSON("/replication/policies", push)
	s.Require().NoError(err)
	s.Equal(http.StatusCreated, res.StatusCode)
}

func (s *replicationAccessTestSuite) TestCreateFailsWhenTheProjectLookupFails() {
	res, err := s.PostJSON("/replication/policies", pullPolicy("broken"))
	s.Require().NoError(err)
	s.Equal(http.StatusInternalServerError, res.StatusCode)
}

func (s *replicationAccessTestSuite) TestCreateForbidden() {
	cases := map[string]struct {
		actions []string
		policy  *models.ReplicationPolicy
	}{
		"without permission":        {nil, pullPolicy("p")},
		"with another action":       {[]string{"read", "list", "update", "delete"}, pullPolicy("p")},
		"into another project":      {[]string{"create"}, pullPolicy("other/app")},
		"into a missing project":    {[]string{"create"}, pullPolicy("gone")},
		"into the source namespace": {[]string{"create"}, pullPolicy("")},
		"push":                      {[]string{"create"}, &models.ReplicationPolicy{Name: "push", DestRegistry: &models.Registry{ID: 3}, DestNamespace: "p"}},
		"between two remotes":       {[]string{"create"}, &models.ReplicationPolicy{Name: "remote", SrcRegistry: &models.Registry{ID: 3}, DestRegistry: &models.Registry{ID: 4}, DestNamespace: "p"}},
		"within Harbor":             {[]string{"create"}, &models.ReplicationPolicy{Name: "local", DestNamespace: "p"}},
	}
	for name, c := range cases {
		s.Run(name, func() {
			s.SetupTest()
			s.grant("/project/7/replication-policy", c.actions...)

			res, err := s.PostJSON("/replication/policies", c.policy)
			s.Require().NoError(err)
			s.Equal(http.StatusForbidden, res.StatusCode)
			s.False(s.called("CreatePolicy"))
		})
	}
}

func storedPull(id int64, destNamespace string) *repctlmodel.Policy {
	return &repctlmodel.Policy{ID: id, Enabled: true, SrcRegistry: &model.Registry{ID: 3}, DestRegistry: &model.Registry{ID: 0}, DestNamespace: destNamespace}
}

// stubStoredPolicies stores pulls into p (5) and into other (6), and a push from p (9). Policy 404 is missing.
// Each policy has an execution with 10 times its ID.
func (s *replicationAccessTestSuite) stubStoredPolicies() {
	s.ctl.On("GetPolicy", mock.Anything, int64(5)).Return(storedPull(5, "p/team"), nil)
	s.ctl.On("GetPolicy", mock.Anything, int64(6)).Return(storedPull(6, "other"), nil)
	s.ctl.On("GetPolicy", mock.Anything, int64(9)).Return(&repctlmodel.Policy{ID: 9, Enabled: true, SrcRegistry: &model.Registry{ID: 0}, DestRegistry: &model.Registry{ID: 3}, DestNamespace: "p"}, nil)
	s.ctl.On("GetPolicy", mock.Anything, int64(404)).Return(nil, errors.NotFoundError(nil))
	for _, id := range []int64{5, 6, 9} {
		s.ctl.On("GetExecution", mock.Anything, id*10).Return(&replication.Execution{ID: id * 10, PolicyID: id}, nil)
	}
	s.ctl.On("GetExecution", mock.Anything, int64(4040)).Return(nil, errors.NotFoundError(nil))
}

type accessCase struct {
	method, path string
	body         any
	resource     string // under /project/7/
	action       string
	calls        string // the controller method that acts
	onExecution  bool   // whether {id} is an execution
}

// id returns the ID in the request about the policy.
func (c accessCase) id(policy int64) int64 {
	if c.onExecution {
		return policy * 10
	}
	return policy
}

var (
	policyCases = []accessCase{
		{http.MethodGet, "/replication/policies/{id}", nil, "replication-policy", "read", "", false},
		{http.MethodDelete, "/replication/policies/{id}", nil, "replication-policy", "delete", "DeletePolicy", false},
		{http.MethodPut, "/replication/policies/{id}", pullPolicy("p/moved"), "replication-policy", "update", "UpdatePolicy", false},
		{http.MethodPost, "/replication/executions", map[string]string{"policy_id": "{id}"}, "replication", "create", "Start", false},
		{http.MethodGet, "/replication/executions?policy_id={id}", nil, "replication", "list", "ListExecutions", false},
	}
	executionCases = []accessCase{
		{http.MethodPut, "/replication/executions/{id}", nil, "replication", "create", "Stop", true},
		{http.MethodGet, "/replication/executions/{id}", nil, "replication", "read", "", true},
		{http.MethodGet, "/replication/executions/{id}/tasks", nil, "replication", "list", "ListTasks", true},
		{http.MethodGet, "/replication/executions/{id}/tasks/1/log", nil, "replication", "read", "GetTaskLog", true},
	}
	allCases = slices.Concat(policyCases, executionCases)
)

func (s *replicationAccessTestSuite) do(c accessCase, id int64) *http.Response {
	path := strings.ReplaceAll(c.path, "{id}", strconv.FormatInt(id, 10))
	var body io.Reader
	if c.body != nil {
		data, err := json.Marshal(c.body)
		s.Require().NoError(err)
		body = strings.NewReader(strings.ReplaceAll(string(data), `"{id}"`, strconv.FormatInt(id, 10)))
	}
	res, err := s.DoReq(c.method, path, body)
	s.Require().NoError(err)
	return res
}

func (s *replicationAccessTestSuite) called(method string) bool {
	return slices.ContainsFunc(s.ctl.Calls, func(call testifymock.Call) bool { return call.Method == method })
}

func (s *replicationAccessTestSuite) TestProjectPermission() {
	for _, c := range allCases {
		s.Run(c.method+" "+c.path, func() {
			s.SetupTest()
			s.stubStoredPolicies()
			s.grant("/project/7/"+c.resource, c.action)

			res := s.do(c, c.id(5))
			s.Less(res.StatusCode, 300)
			if c.calls != "" {
				s.True(s.called(c.calls), "calls %s", c.calls)
			}
			for _, call := range s.ctl.Calls {
				switch call.Method {
				case "ExecutionCount", "ListExecutions":
					s.Equal(int64(5), call.Arguments.Get(1).(*q.Query).Keywords["PolicyID"])
				case "TaskCount", "ListTasks":
					s.Equal(int64(50), call.Arguments.Get(1).(*q.Query).Keywords["ExecutionID"])
				}
			}
		})
	}
}

func (s *replicationAccessTestSuite) TestUnauthenticated() {
	s.Security.ExpectedCalls = nil
	s.Security.On("IsAuthenticated").Return(false)
	s.Security.On("GetUsername").Return("")
	s.stubStoredPolicies()

	cases := append(slices.Clone(allCases),
		accessCase{method: http.MethodPost, path: "/replication/policies", body: pullPolicy("p")},
		accessCase{method: http.MethodGet, path: "/replication/policies"})
	for _, c := range cases {
		s.Equal(http.StatusUnauthorized, s.do(c, c.id(5)).StatusCode, "%s %s", c.method, c.path)
	}
	s.Empty(s.ctl.Calls)
}

func (s *replicationAccessTestSuite) TestLogOfAnotherExecutionsTaskIsNotFound() {
	s.stubStoredPolicies()
	s.ctl.On("GetTask", mock.Anything, int64(2)).Return(&replication.Task{ID: 2, ExecutionID: 60}, nil)
	s.grant("/project/7/replication", "read")

	res, err := s.Get("/replication/executions/50/tasks/2/log")
	s.Require().NoError(err)
	s.Equal(http.StatusNotFound, res.StatusCode)
	s.False(s.called("GetTaskLog"))
}

func (s *replicationAccessTestSuite) TestProjectPermissionForbidden() {
	allOtherActions := func(c accessCase) {
		for _, resource := range []string{"replication-policy", "replication"} {
			for _, action := range []string{"create", "read", "list", "update", "delete"} {
				if resource != c.resource || action != c.action {
					s.grant("/project/7/"+resource, action)
				}
			}
		}
	}
	theAction := func(c accessCase) { s.grant("/project/7/"+c.resource, c.action) }
	targets := map[string]struct {
		policy int64
		grant  func(accessCase)
	}{
		"without the permission":         {5, allOtherActions},
		"on a pull into another project": {6, theAction},
		"on a push from the project":     {9, theAction},
	}
	for name, target := range targets {
		for _, c := range allCases {
			s.Run(name+" "+c.method+" "+c.path, func() {
				s.SetupTest()
				s.stubStoredPolicies()
				target.grant(c)

				res := s.do(c, c.id(target.policy))
				s.Equal(http.StatusForbidden, res.StatusCode)
				if c.calls != "" {
					s.False(s.called(c.calls), "calls %s", c.calls)
				}
			})
		}
	}
}

func (s *replicationAccessTestSuite) TestSystemPermissionDoesNotGetPolicies() {
	for _, c := range allCases {
		// These handlers get the policy to act on it.
		if c.calls == "Start" || c.method == http.MethodGet && c.path == "/replication/policies/{id}" {
			continue
		}
		id := int64(404)
		if c.onExecution {
			id = 50
		}
		s.Run(c.method+" "+c.path, func() {
			s.SetupTest()
			s.ctl.On("GetExecution", mock.Anything, int64(50)).Return(&replication.Execution{ID: 50, PolicyID: 404}, nil)
			s.grant("/system/"+c.resource, c.action)

			res := s.do(c, id)
			s.Less(res.StatusCode, 300)
			s.False(s.called("GetPolicy"))
		})
	}
}

func (s *replicationAccessTestSuite) TestUpdateForbiddenIntoAnotherProject() {
	s.stubStoredPolicies()
	s.grant("/project/7/replication-policy", "update")

	res := s.do(accessCase{method: http.MethodPut, path: "/replication/policies/{id}", body: pullPolicy("other")}, 5)
	s.Equal(http.StatusForbidden, res.StatusCode)
	s.False(s.called("UpdatePolicy"))
}

func (s *replicationAccessTestSuite) TestListExecutionsOfAllPoliciesNeedsSystemPermission() {
	s.grant("/project/7/replication", "list")

	res, err := s.Get("/replication/executions")
	s.Require().NoError(err)
	s.Equal(http.StatusForbidden, res.StatusCode)
}

func (s *replicationAccessTestSuite) TestExecutionsOfMissingPolicyAreEmpty() {
	s.stubStoredPolicies()
	s.grant("/project/7/replication", "list")

	res, err := s.Get("/replication/executions?policy_id=404")
	s.Require().NoError(err)
	s.Equal(http.StatusOK, res.StatusCode)
	s.Equal("0", res.Header.Get("X-Total-Count"))
	s.False(s.called("ListExecutions"))
}

func (s *replicationAccessTestSuite) TestListExecutionsFailsWhenThePolicyLookupFails() {
	s.ctl.On("GetPolicy", mock.Anything, int64(77)).Return(nil, errors.New("database is down"))
	s.grant("/project/7/replication", "list")

	res, err := s.Get("/replication/executions?policy_id=77")
	s.Require().NoError(err)
	s.Equal(http.StatusInternalServerError, res.StatusCode)
}

func (s *replicationAccessTestSuite) TestMissingIsNotFound() {
	for _, c := range allCases {
		// A missing policy has no executions to list.
		if c.calls == "ListExecutions" {
			continue
		}
		s.Run(c.method+" "+c.path, func() {
			s.SetupTest()
			s.stubStoredPolicies()
			s.grant("/project/7/"+c.resource, c.action)

			res := s.do(c, c.id(404))
			s.Equal(http.StatusNotFound, res.StatusCode)
		})
	}
}

func (s *replicationAccessTestSuite) listPolicies(path string) ([]*models.ReplicationPolicy, *http.Response) {
	var policies []*models.ReplicationPolicy
	res, err := s.GetJSON(path, &policies)
	s.Require().NoError(err)
	return policies, res
}

// listAsRobot lists policies as a robot at the level with the access to project p.
func (s *replicationAccessTestSuite) listAsRobot(level, rawQuery string, access ...*types.Policy) ([]*models.ReplicationPolicy, *httptest.ResponseRecorder) {
	projectCtlMock.On("Get", mock.Anything, int64(7), mock.Anything).Return(&project.Project{ProjectID: 7, Name: "p"}, nil)
	projectCtlMock.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(nil, errors.NotFoundError(nil))
	defer func(ctl project.Controller) { project.Ctl = ctl }(project.Ctl)
	project.Ctl = projectCtlMock
	sc := robotsec.NewSecurityContext(&robot.Robot{Robot: robotmodel.Robot{Name: "robot$replicator"}, Level: level, Permissions: []*robot.Permission{
		{Kind: robot.LEVELPROJECT, Namespace: "p", Scope: "/project/7", Access: access},
	}})

	h, _, err := restapi.HandlerAPI(*s.Config)
	s.Require().NoError(err)
	req := httptest.NewRequest(http.MethodGet, "/api/v2.0/replication/policies?"+rawQuery, nil)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req.WithContext(security.NewContext(req.Context(), sc)))

	var policies []*models.ReplicationPolicy
	if res.Code == http.StatusOK {
		s.Require().NoError(json.Unmarshal(res.Body.Bytes(), &policies))
	}
	return policies, res
}

func policyIDs(policies []*models.ReplicationPolicy) []int64 {
	ids := []int64{}
	for _, p := range policies {
		ids = append(ids, p.ID)
	}
	return ids
}

func (s *replicationAccessTestSuite) TestListWithProjectPermission() {
	push := &repctlmodel.Policy{ID: 3, SrcRegistry: &model.Registry{ID: 0}, DestRegistry: &model.Registry{ID: 3}, DestNamespace: "p"}
	s.ctl.On("ListPolicies", mock.Anything, mock.Anything).Return(
		[]*repctlmodel.Policy{storedPull(1, "p/a"), storedPull(2, "other"), push, storedPull(4, "p"), storedPull(5, "gone"), storedPull(6, "p/b"), storedPull(7, "other/b")}, nil)

	policies, res := s.listAsRobot(robot.LEVELSYSTEM, "name=pull&sort=-id&page=2&page_size=2", &types.Policy{Resource: "replication-policy", Action: "list"})
	s.Equal(http.StatusOK, res.Code)
	s.Equal([]int64{6}, policyIDs(policies))
	s.Equal("3", res.Header().Get("X-Total-Count"))
	s.Contains(res.Header().Get("Link"), `rel="prev"`)

	query := s.ctl.Calls[0].Arguments.Get(1).(*q.Query)
	s.Zero(query.PageSize)
	s.Equal(0, query.Keywords["DestRegistryID"])
	s.Equal(&q.FuzzyMatchValue{Value: "pull"}, query.Keywords["Name"])
	s.Equal([]*q.Sort{{Key: "id", DESC: true}}, query.Sorts)
	projectCtlMock.AssertNumberOfCalls(s.T(), "GetByName", 3)
}

func (s *replicationAccessTestSuite) TestListFailsWhenTheProjectLookupFails() {
	s.ctl.On("ListPolicies", mock.Anything, mock.Anything).Return([]*repctlmodel.Policy{storedPull(1, "broken")}, nil)

	_, res := s.listAsRobot(robot.LEVELSYSTEM, "", &types.Policy{Resource: "replication-policy", Action: "list"})
	s.Equal(http.StatusInternalServerError, res.Code)
}

func (s *replicationAccessTestSuite) TestListWithoutPermission() {
	s.ctl.On("ListPolicies", mock.Anything, mock.Anything).Return([]*repctlmodel.Policy{storedPull(1, "p")}, nil)

	policies, res := s.listAsRobot(robot.LEVELSYSTEM, "", &types.Policy{Resource: "replication-policy", Action: "read"})
	s.Equal(http.StatusOK, res.Code)
	s.Empty(policies)
	s.Equal("0", res.Header().Get("X-Total-Count"))
}

func (s *replicationAccessTestSuite) TestListAsProjectRobotIsForbidden() {
	_, res := s.listAsRobot(robot.LEVELPROJECT, "", &types.Policy{Resource: "replication-policy", Action: "list"})
	s.Equal(http.StatusForbidden, res.Code)
	s.False(s.called("ListPolicies"))
}

func (s *replicationAccessTestSuite) TestListAsAnotherCallerIsForbidden() {
	s.Security.On("Name").Return("local")
	s.grant("/project/7/replication-policy", "list")

	_, res := s.listPolicies("/replication/policies")
	s.Equal(http.StatusForbidden, res.StatusCode)
	s.False(s.called("ListPolicies"))
}

func (s *replicationAccessTestSuite) TestListWithSystemPermission() {
	s.grant("/system/replication-policy", "list")
	s.ctl.On("PolicyCount", mock.Anything, mock.Anything).Return(int64(11), nil)
	s.ctl.On("ListPolicies", mock.Anything, testifymock.MatchedBy(func(query *q.Query) bool { return query.PageSize == 10 })).Return(
		[]*repctlmodel.Policy{storedPull(2, "other")}, nil)

	policies, res := s.listPolicies("/replication/policies")
	s.Equal(http.StatusOK, res.StatusCode)
	s.Equal([]int64{2}, policyIDs(policies))
	s.Equal("11", res.Header.Get("X-Total-Count"))
}

func TestPullProject(t *testing.T) {
	remote, local := &model.Registry{ID: 3}, &model.Registry{ID: 0}
	cases := map[string]struct {
		policy  *repctlmodel.Policy
		project string
		pulls   bool
	}{
		"a pull":                            {&repctlmodel.Policy{SrcRegistry: remote, DestRegistry: local, DestNamespace: "p/a"}, "p", true},
		"a requested pull":                  {&repctlmodel.Policy{SrcRegistry: remote, DestNamespace: "p"}, "p", true},
		"a pull into the source namespaces": {&repctlmodel.Policy{SrcRegistry: remote, DestRegistry: local}, "", false},
		"a push":                            {&repctlmodel.Policy{SrcRegistry: local, DestRegistry: remote, DestNamespace: "p"}, "", false},
		"between remotes":                   {&repctlmodel.Policy{SrcRegistry: remote, DestRegistry: &model.Registry{ID: 4}, DestNamespace: "p"}, "", false},
		"within Harbor":                     {&repctlmodel.Policy{DestNamespace: "p"}, "", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			project, pulls := pullProject(c.policy)
			assert.Equal(t, c.project, project)
			assert.Equal(t, c.pulls, pulls)
		})
	}
}

func TestPage(t *testing.T) {
	items := []int{1, 2, 3}
	assert.Equal(t, []int{1, 2, 3}, page(items, 1, 0))
	assert.Equal(t, []int{1, 2}, page(items, 0, 2))
	assert.Equal(t, []int{3}, page(items, 2, 2))
	assert.Empty(t, page(items, 3, 2))
	assert.Empty(t, page(items, math.MaxInt64, 100))
}

func TestReplicationAccessTestSuite(t *testing.T) {
	suite.Run(t, &replicationAccessTestSuite{})
}
