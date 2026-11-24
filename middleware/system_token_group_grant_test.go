package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystemTokenGroupGrantRequiresExactHiddenKeyAndGroups(t *testing.T) {
	grant := systemTokenGroupGrant{UserID: 41, TokenID: 51, UserGroup: "existing", Group: "workstation"}
	for _, scenario := range []struct {
		name, currentGroup, tokenGroup string
		owner, id                      int
		hidden, want                   bool
	}{
		{"指定系统 Key", "existing", "workstation", 41, 51, true, true},
		{"同一用户的其他系统 Key", "existing", "workstation", 41, 52, true, false},
		{"非隐藏 Key", "existing", "workstation", 41, 51, false, false},
		{"不同账号", "existing", "workstation", 42, 51, true, false},
		{"账号分组已变更", "changed", "workstation", 41, 51, true, false},
		{"Key 改到其他分组", "existing", "other", 41, 51, true, false},
		{"自动分组", "existing", "auto", 41, 51, true, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			token := &model.Token{UserId: scenario.owner, Id: scenario.id, Hidden: scenario.hidden, Group: scenario.tokenGroup}
			assert.Equal(t, scenario.want, grant.matches(token, scenario.currentGroup))
			assert.False(t, (systemTokenGroupGrant{}).matches(token, scenario.currentGroup), "默认没有附加授权")
		})
	}
}

func TestTokenAuthOnlyGrantAllowsMatchingSystemKeyWithoutSkippingValidation(t *testing.T) {
	setupDashboardAuthMiddlewareTest(t)
	model.InitCol()
	require.NoError(t, i18n.Init())
	require.NoError(t, model.DB.AutoMigrate(&model.Token{}))
	ownerGroup, grantGroup := "grant-test-owner", "grant-test-workstation"
	oldRatios := ratio_setting.GroupRatio2JSONString()
	t.Cleanup(func() { assert.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(oldRatios)) })
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"grant-test-owner":1,"grant-test-workstation":1}`))
	owner := model.User{Username: "grant-test-user", Password: "test-password", Role: common.RoleCommonUser,
		Status: common.UserStatusEnabled, Group: ownerGroup, AffCode: "grant-test-code"}
	require.NoError(t, model.DB.Create(&owner).Error)
	keys := []*model.Token{
		{Key: strings.Repeat("a", 48), UserId: owner.Id, Group: grantGroup, Hidden: true, ExpiredTime: -1, UnlimitedQuota: true},
		{Key: strings.Repeat("b", 48), UserId: owner.Id, Group: grantGroup, Hidden: true, ExpiredTime: -1, UnlimitedQuota: true},
		{Key: strings.Repeat("c", 48), UserId: owner.Id, Group: ownerGroup, ExpiredTime: -1, UnlimitedQuota: true},
		{Key: strings.Repeat("d", 48), UserId: owner.Id, Group: grantGroup, Hidden: true, ExpiredTime: 1, UnlimitedQuota: true},
	}
	for _, key := range keys {
		require.NoError(t, model.DB.Create(key).Error)
	}
	request := func(key *model.Token, grant systemTokenGroupGrant) *httptest.ResponseRecorder {
		router := gin.New()
		router.GET("/v1/models", tokenAuthWithSystemGroupGrant(grant), func(c *gin.Context) {
			c.String(http.StatusOK, common.GetContextKeyString(c, constant.ContextKeyUsingGroup))
		})
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer sk-"+key.Key)
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		return res
	}
	grant := systemTokenGroupGrant{UserID: owner.Id, TokenID: keys[0].Id, UserGroup: ownerGroup, Group: grantGroup}
	assert.Equal(t, http.StatusForbidden, request(keys[0], systemTokenGroupGrant{}).Code, "未配置时保持原有拒绝")
	response := request(keys[0], grant)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, grantGroup, response.Body.String(), "授权后的请求路由到既有工作台分组")
	assert.Equal(t, http.StatusForbidden, request(keys[1], grant).Code, "原用户另一把 Key 不获授权")
	assert.Equal(t, ownerGroup, request(keys[2], grant).Body.String(), "普通 Key 仍访问原分组")
	expiredGrant := grant
	expiredGrant.TokenID = keys[3].Id
	assert.Equal(t, http.StatusUnauthorized, request(keys[3], expiredGrant).Code, "命中授权仍不能使用已过期 Key")
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"grant-test-owner":1}`))
	assert.Equal(t, http.StatusForbidden, request(keys[0], grant).Code, "目标分组被停用时仍拒绝")
}
