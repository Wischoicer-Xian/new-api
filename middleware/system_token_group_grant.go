package middleware

import (
	"os"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

const systemTokenGroupGrantEnv = "WISCHOICER_SYSTEM_TOKEN_GROUP_GRANT"

// 授权只对当前声明中的一把系统 Key 生效，不修改账号或共享分组。
type systemTokenGroupGrant struct {
	UserID    int    `json:"user_id"`
	TokenID   int    `json:"token_id"`
	UserGroup string `json:"user_group"`
	Group     string `json:"group"`
}

var configuredSystemTokenGroupGrant = sync.OnceValue(func() systemTokenGroupGrant {
	raw := strings.TrimSpace(os.Getenv(systemTokenGroupGrantEnv))
	if raw == "" {
		return systemTokenGroupGrant{}
	}
	var grant systemTokenGroupGrant
	if err := common.UnmarshalJsonStr(raw, &grant); err != nil || !grant.valid() {
		common.SysError(common.LogText("invalid Wischoicer system token group grant; grant ignored"))
		return systemTokenGroupGrant{}
	}
	return grant
})

func (g systemTokenGroupGrant) valid() bool {
	return g.UserID > 0 && g.TokenID > 0 && strings.TrimSpace(g.UserGroup) != "" &&
		strings.TrimSpace(g.Group) != "" && g.Group != "auto" && g.Group != g.UserGroup
}

func (g systemTokenGroupGrant) matches(token *model.Token, userGroup string) bool {
	return g.valid() && token.Hidden && token.Id == g.TokenID && token.UserId == g.UserID &&
		userGroup == g.UserGroup && token.Group == g.Group
}
