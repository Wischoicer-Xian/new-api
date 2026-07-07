package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── WIS-460 preflight 命门测试（记星 R2 review 核心证据）───
//
// 锁死不变式：预检纯读——调用 ComputePreflightQuote / resolveFundingReadOnly 前后，
// user.quota / token.remain_quota / subscription.amount_used 恒等。
// 任何 preConsume / Decrease* / PreConsumeUserSubscription 路径被误触即本测试红。

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

// TestComputePreflightQuote_NoMutation：全链预检（ModelPriceHelper 只读 + funding 只读）后三处余额恒等。
// 无论模型是否配价（reason 可能是 ok / insufficient / model_price_not_configured），核心断言是「不变」。
func TestComputePreflightQuote_NoMutation(t *testing.T) {
	truncate(t)
	const uid = 1001
	seedUser(t, uid, 5000)
	seedToken(t, 2001, uid, "sk-quote", 9999)
	seedSubscription(t, 3001, uid, 100000, 20000)

	userBefore := getUserQuota(t, uid)
	tokenBefore := getTokenRemainQuota(t, 2001)
	subBefore := getSubAmountUsed(t, 3001)

	res := ComputePreflightQuote(quoteTestCtx(), uid, QuoteInput{
		Model: "gpt-image-2", Prompt: "a summer dress", Size: "1024x1024",
	})
	require.NotNil(t, res)
	// reason 任意（取决于模型是否配价）；断言是合法码，不断言具体值。
	assert.Contains(t, []string{"ok", "insufficient_user_quota", "model_price_not_configured", "internal_error"}, res.Reason)

	// 命门：三处余额前后恒等。
	assert.Equal(t, userBefore, getUserQuota(t, uid), "user.quota 不可变（预检纯读）")
	assert.Equal(t, tokenBefore, getTokenRemainQuota(t, 2001), "token.remain_quota 不可变（预检纯读）")
	assert.Equal(t, subBefore, getSubAmountUsed(t, 3001), "subscription.amount_used 不可变（预检纯读）")
}

// TestResolveFundingReadOnly_DecisionsAndNoMutation：直接测 funding resolver（复刻 NewBillingSession switch
// 只读版），覆盖 wallet够/不够 + subscription够 + 无订阅回退钱包；同时锁「读 balance 后不可变」。
func TestResolveFundingReadOnly_DecisionsAndNoMutation(t *testing.T) {
	truncate(t)
	const uid = 1002
	seedUser(t, uid, 1000)                       // 钱包 1000
	seedSubscription(t, 3002, uid, 50000, 10000) // 订阅剩余 40000

	t.Run("wallet_only 够 -> wallet/ok，余额不变", func(t *testing.T) {
		qBefore := getUserQuota(t, uid)
		f, cur, ok, r := resolveFundingReadOnly(uid, "wallet_only", 500)
		assert.Equal(t, "wallet", f)
		assert.True(t, ok)
		assert.Equal(t, "ok", r)
		assert.Equal(t, 1000, cur)
		assert.Equal(t, qBefore, getUserQuota(t, uid), "wallet 读后不可变")
	})

	t.Run("wallet_only 不够 -> wallet/insufficient_user_quota，余额不变", func(t *testing.T) {
		f, cur, ok, r := resolveFundingReadOnly(uid, "wallet_only", 2000)
		assert.Equal(t, "wallet", f)
		assert.False(t, ok)
		assert.Equal(t, "insufficient_user_quota", r)
		assert.Equal(t, 1000, cur)
		assert.Equal(t, 1000, getUserQuota(t, uid), "wallet 读后不可变（不可因预扣变少）")
	})

	t.Run("subscription_only 够 -> subscription/ok，amount_used 不变", func(t *testing.T) {
		usedBefore := getSubAmountUsed(t, 3002)
		f, cur, ok, r := resolveFundingReadOnly(uid, "subscription_only", 30000)
		assert.Equal(t, "subscription", f)
		assert.True(t, ok)
		assert.Equal(t, "ok", r)
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

	f, cur, ok, r := resolveFundingReadOnly(uid, "subscription_first", 300)
	assert.Equal(t, "wallet", f, "无活跃订阅 → subscription_first 应回退 wallet")
	assert.True(t, ok)
	assert.Equal(t, "ok", r)
	assert.Equal(t, 800, cur)
	assert.Equal(t, 800, getUserQuota(t, uid), "wallet 读后不可变")
}
