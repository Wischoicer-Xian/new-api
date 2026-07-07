package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── WIS-460 preflight 命门测试（记星 R2 review 核心证据）───
//
// 锁死两件事：
//  1. 预检纯读——调用前后 user.quota / token.remain_quota / subscription.amount_used 恒等。
//  2. 预检口径 == 真实预扣——订阅按 PreConsumeUserSubscription 单订阅语义（不跨订阅求和；无限订阅覆盖）。
//
// 任何 preConsume / Decrease* / PreConsumeUserSubscription 被误触即本测试红。

func getSubAmountUsed(t *testing.T, id int) int64 {
	t.Helper()
	var sub model.UserSubscription
	require.NoError(t, model.DB.Select("amount_used").Where("id = ?", id).First(&sub).Error)
	return sub.AmountUsed
}

func quoteTestCtx() *gin.Context {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}

// TestComputePreflightQuote_NoMutation（记星 R2 P1#3）：必须真跑到成功路径——给 gpt-image-2 配价
// 让 ModelPriceHelper 算出 required>0、走 funding 分支，断言 reason==ok（非 internal_error 空过），
// 再锁三处余额前后恒等。
func TestComputePreflightQuote_NoMutation(t *testing.T) {
	truncate(t)
	// 配价（usePrice）→ required>0，强制全链真跑过 ModelPriceHelper + funding（否则会因模型未配价早退）。
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"gpt-image-2":0.05}`))
	t.Cleanup(func() { _ = ratio_setting.UpdateModelPriceByJSONString(`{}`) })

	const uid = 1001
	seedUser(t, uid, 500000) // 钱包充足
	seedToken(t, 2001, uid, "sk-quote", 9999)
	seedSubscription(t, 3001, uid, 100000, 20000) // 单张订阅剩余 80000

	userBefore := getUserQuota(t, uid)
	tokenBefore := getTokenRemainQuota(t, 2001)
	subBefore := getSubAmountUsed(t, 3001)

	// 真实 billing 输入镜像（body + headers），不截断 tiered 依赖。
	res := ComputePreflightQuote(quoteTestCtx(), uid, QuoteInput{
		Model:   "gpt-image-2",
		Body:    []byte(`{"model":"gpt-image-2","prompt":"a summer dress","size":"1024x1024"}`),
		Headers: map[string]string{"X-Request-Id": "preflight-test"},
	})
	require.NotNil(t, res)
	assert.Equal(t, "ok", res.Reason, "必须真跑到成功路径，不允许 internal_error/SQL error 空过")
	assert.True(t, res.CanStart)
	assert.Greater(t, res.RequiredQuota, 0, "配价后 required>0，证明走过 ModelPriceHelper")

	// 命门：三处余额前后恒等。
	assert.Equal(t, userBefore, getUserQuota(t, uid), "user.quota 不可变（预检纯读）")
	assert.Equal(t, tokenBefore, getTokenRemainQuota(t, 2001), "token.remain_quota 不可变（预检纯读）")
	assert.Equal(t, subBefore, getSubAmountUsed(t, 3001), "subscription.amount_used 不可变（预检纯读）")
}

// TestResolveFundingReadOnly_DecisionsAndNoMutation：wallet够/不够 + 单订阅够，各分支「读 balance 后不可变」。
func TestResolveFundingReadOnly_DecisionsAndNoMutation(t *testing.T) {
	truncate(t)
	const uid = 1002
	seedUser(t, uid, 1000)                       // 钱包 1000
	seedSubscription(t, 3002, uid, 50000, 10000) // 单张订阅剩余 40000

	t.Run("wallet_only 够 -> wallet/ok，余额不变", func(t *testing.T) {
		qBefore := getUserQuota(t, uid)
		f, cur, ok, r, un := resolveFundingReadOnly(uid, "wallet_only", 500)
		assert.Equal(t, "wallet", f)
		assert.True(t, ok)
		assert.Equal(t, "ok", r)
		assert.False(t, un)
		assert.Equal(t, 1000, cur)
		assert.Equal(t, qBefore, getUserQuota(t, uid), "wallet 读后不可变")
	})

	t.Run("wallet_only 不够 -> wallet/insufficient_user_quota，余额不变", func(t *testing.T) {
		f, cur, ok, r, _ := resolveFundingReadOnly(uid, "wallet_only", 2000)
		assert.Equal(t, "wallet", f)
		assert.False(t, ok)
		assert.Equal(t, "insufficient_user_quota", r)
		assert.Equal(t, 1000, cur)
		assert.Equal(t, 1000, getUserQuota(t, uid), "wallet 读后不可变（不可因预扣变少）")
	})

	t.Run("subscription_only 单张够 -> subscription/ok，amount_used 不变", func(t *testing.T) {
		usedBefore := getSubAmountUsed(t, 3002)
		f, cur, ok, r, un := resolveFundingReadOnly(uid, "subscription_only", 30000)
		assert.Equal(t, "subscription", f)
		assert.True(t, ok)
		assert.Equal(t, "ok", r)
		assert.False(t, un)
		assert.Equal(t, 40000, cur)
		assert.Equal(t, usedBefore, getSubAmountUsed(t, 3002), "subscription.amount_used 读后不可变（不可预扣）")
	})
}

// TestResolveFundingReadOnly_NoSubscriptionFallsBackToWallet：subscription_first 且无活跃订阅 →
// 回退 wallet（与 NewBillingSession :422-423 一致）；独占 truncate 避免与同表 username 冲突。
func TestResolveFundingReadOnly_NoSubscriptionFallsBackToWallet(t *testing.T) {
	truncate(t)
	const uid = 1003
	seedUser(t, uid, 800) // 无订阅

	f, cur, ok, r, _ := resolveFundingReadOnly(uid, "subscription_first", 300)
	assert.Equal(t, "wallet", f, "无活跃订阅 → subscription_first 应回退 wallet")
	assert.True(t, ok)
	assert.Equal(t, "ok", r)
	assert.Equal(t, 800, cur)
	assert.Equal(t, 800, getUserQuota(t, uid), "wallet 读后不可变")
}

// TestResolveFundingReadOnly_SubscriptionSingleSubNoSum（记星 R2 P1#1）：多订阅拆额——单张都不够、
// 合计够时，can_start 必须 false（真实 PreConsumeUserSubscription 锁单张、不跨订阅求和）。
func TestResolveFundingReadOnly_SubscriptionSingleSubNoSum(t *testing.T) {
	truncate(t)
	const uid = 1004
	seedUser(t, uid, 0) // 钱包 0，强制走订阅
	// 两张订阅，单张剩余 15000 < need 20000，合计 30000 够。
	seedSubscription(t, 4001, uid, 20000, 5000)
	seedSubscription(t, 4002, uid, 20000, 5000)

	f, _, ok, r, _ := resolveFundingReadOnly(uid, "subscription_only", 20000)
	assert.Equal(t, "subscription", f)
	assert.False(t, ok, "真实预扣锁单张、不跨订阅求和；单张不够即 can_start=false")
	assert.Equal(t, "insufficient_user_quota", r)
}

// TestResolveFundingReadOnly_SubscriptionUnlimited（记星 R2 P1#1）：AmountTotal<=0 的无限订阅
// 应覆盖任意 need（与 PreConsumeUserSubscription 跳过 remain 校验一致），can_start=true + unlimited。
func TestResolveFundingReadOnly_SubscriptionUnlimited(t *testing.T) {
	truncate(t)
	const uid = 1005
	seedUser(t, uid, 0)
	seedSubscription(t, 4003, uid, 0, 0) // AmountTotal=0 → 无限订阅

	f, _, ok, r, un := resolveFundingReadOnly(uid, "subscription_only", 20000)
	assert.Equal(t, "subscription", f)
	assert.True(t, ok, "无限订阅（AmountTotal<=0）覆盖任意 need")
	assert.Equal(t, "ok", r)
	assert.True(t, un, "应识别为 unlimited")
}
