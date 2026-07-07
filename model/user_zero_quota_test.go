package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewUserStartsAtZeroQuota（WIS-460 Phase-B，Jirui 裁决：代码不给新用户默认额度）锁两件源头契约：
//
//	(a) common.QuotaForNewUser 代码默认 == 0；
//	(b) User.Insert 强制 user.Quota = QuotaForNewUser（admin 建号不读 body quota——即便 body 带 9999 也被覆盖）。
//
// 消费侧 preflight（content-workstation）只锁「给定 0 额度→拦」，管不到 new-api 源头；本测试把命门
// 从人盯变机器盯：有人把 QuotaForNewUser 默认改 >0、或在建号路径塞默认授予，本测试即红。
//
// 不空过：真跑过 Insert 成功路径 + DB 回读确认 quota 落 0（吸取 preflight Layer 1 internal_error 空过教训）。
// 不测 DB option 覆盖：QuotaForNewUser option 是运维权限（dashboard 可设），属运营操作、非代码契约，
// Jirui 要的是「代码侧默认 0」，option 覆盖不在本测试范围。
func TestNewUserStartsAtZeroQuota(t *testing.T) {
	// (a) 代码默认必须 == 0。
	require.Equal(t, 0, common.QuotaForNewUser,
		"common.QuotaForNewUser 代码默认必须 == 0（Jirui 裁决：代码不给新用户默认额度）")

	// (b) admin 建号不读 body quota：给一个非零 body quota（9999），验 Insert 覆盖成 QuotaForNewUser(=0)。
	user := &User{
		Username:    "wis460_zero_quota", // unique（≤20）
		Password:    "password123",       // 8-20，Insert 会 hash
		DisplayName: "wis460",
		Quota:       9999, // 模拟 body 带的额度 —— Insert 应强制覆盖成 QuotaForNewUser
	}
	require.NoError(t, user.Insert(0), "Insert 必须成功（不空过：真跑过建号路径）")

	// 入参 user.Quota 被 Insert 改写成 QuotaForNewUser（不是 body 的 9999）。
	assert.Equal(t, common.QuotaForNewUser, user.Quota,
		"Insert 必须把 quota 强制成 QuotaForNewUser，不能读 body 的 9999")

	// 从 DB 回读，确认落库 == 0（不是空过：真写到 DB 了，而非只在内存断言）。
	var fetched User
	require.NoError(t, DB.Where("username = ?", "wis460_zero_quota").First(&fetched).Error,
		"回读建号用户必须成功")
	assert.Equal(t, 0, fetched.Quota, "新用户落库 quota 必须 == 0（Jirui 裁决：新用户起步 0 计费额度）")
}
