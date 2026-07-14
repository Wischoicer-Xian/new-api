package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIsWischoicerRechargeAllowedAmountCents 守护「服务端权威金额白名单」这一资金不变量
// （WIS-547 §1 / WIS-550）：普通用户仅四档可达，¥1 仅白名单测试账号可达，其余一律拒绝。
func TestIsWischoicerRechargeAllowedAmountCents(t *testing.T) {
	orig := WischoicerRechargeTestUserIDs
	t.Cleanup(func() { WischoicerRechargeTestUserIDs = orig })

	// 默认无白名单：四档对所有人可用；¥1 与任意非档位金额对所有人不可用。
	WischoicerRechargeTestUserIDs = map[int]struct{}{}
	for _, cents := range WischoicerRechargeTierCents {
		assert.True(t, IsWischoicerRechargeAllowedAmountCents(cents, 42), "production tier must be allowed: %d", cents)
	}
	for _, cents := range []int64{100, 0, -1, 30000, 9999, 100000} {
		assert.False(t, IsWischoicerRechargeAllowedAmountCents(cents, 42), "non-tier amount must be rejected: %d", cents)
	}

	// 白名单测试账号 42：¥1 可达；非白名单账号 43：¥1 仍拒绝。
	WischoicerRechargeTestUserIDs = map[int]struct{}{42: {}}
	assert.True(t, IsWischoicerRechargeAllowedAmountCents(100, 42))
	assert.False(t, IsWischoicerRechargeAllowedAmountCents(100, 43))
}

func TestParseWischoicerRechargeTestUserIDs(t *testing.T) {
	assert.Empty(t, parseWischoicerRechargeTestUserIDs(""))
	assert.Empty(t, parseWischoicerRechargeTestUserIDs(" , , "))

	ids := parseWischoicerRechargeTestUserIDs("42, 7, 0, abc, 99")
	_, has42 := ids[42]
	_, has7 := ids[7]
	_, has99 := ids[99]
	assert.True(t, has42)
	assert.True(t, has7)
	assert.True(t, has99)
	assert.Len(t, ids, 3, "invalid entries (0, abc) must be skipped")
}
