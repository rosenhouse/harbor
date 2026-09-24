package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/suite"

	commonmodels "github.com/goharbor/harbor/src/common/models"
	"github.com/goharbor/harbor/src/common/rbac"
	"github.com/goharbor/harbor/src/common/security/local"
	"github.com/goharbor/harbor/src/controller/member"
	"github.com/goharbor/harbor/src/server/v2.0/models"
	"github.com/goharbor/harbor/src/server/v2.0/restapi"
	htesting "github.com/goharbor/harbor/src/testing/server/v2.0/handler"
)

type projectAdminMemberController struct {
	member.Controller
}

func (projectAdminMemberController) IsProjectAdmin(context.Context, commonmodels.User) (bool, error) {
	return true, nil
}

type PermissionsTestSuite struct {
	htesting.Suite
}

func (suite *PermissionsTestSuite) SetupSuite() {
	suite.Config = &restapi.Config{PermissionsAPI: &permissionsAPI{mc: projectAdminMemberController{}}}
	suite.Suite.SetupSuite()
}

func (suite *PermissionsTestSuite) SetupTest() {
	suite.Caller = nil
}

func (suite *PermissionsTestSuite) getPermissions() *models.Permissions {
	var perms models.Permissions
	res, err := suite.GetJSON("/permissions", &perms)
	suite.Require().NoError(err)
	suite.Require().Equal(http.StatusOK, res.StatusCode)
	for _, p := range rbac.SystemRobotProjectPolicies {
		suite.NotContains(perms.Project, &models.Permission{Resource: p.Resource.String(), Action: p.Action.String()})
	}
	return &perms
}

func (suite *PermissionsTestSuite) TestSystemAdminGetsSystemRobotProjectPermissions() {
	suite.Caller = local.NewSecurityContext(&commonmodels.User{UserID: 1, SysAdminFlag: true})

	perms := suite.getPermissions()

	var want []*models.Permission
	for _, p := range rbac.SystemRobotProjectPolicies {
		want = append(want, &models.Permission{Resource: p.Resource.String(), Action: p.Action.String()})
	}
	suite.Equal(want, perms.SystemRobotProject)
}

func (suite *PermissionsTestSuite) TestProjectAdminDoesNotGetSystemRobotProjectPermissions() {
	suite.Caller = local.NewSecurityContext(&commonmodels.User{UserID: 2})

	perms := suite.getPermissions()

	suite.NotEmpty(perms.Project)
	suite.Empty(perms.System)
	suite.Empty(perms.SystemRobotProject)
}

func TestPermissionsTestSuite(t *testing.T) {
	suite.Run(t, &PermissionsTestSuite{})
}
