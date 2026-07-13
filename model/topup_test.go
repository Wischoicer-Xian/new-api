package model

import (
	"math"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// truncateTopUpTables 清理 topup 测试涉及的表。top_ups + users 是核心；
// wischoicer_recharge_credits 在溢出降级路径会被 sumActiveReservedQuotaTx 汇总，
// 一并清理避免跨用例串扰。
func truncateTopUpTables(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		DB.Exec("DELETE FROM top_ups")
		DB.Exec("DELETE FROM wischoicer_recharge_credits")
		DB.Exec("DELETE FROM users")
	})
}

func seedTopUpRow(t *testing.T, topUp *TopUp) *TopUp {
	t.Helper()
	require.NoError(t, DB.Create(topUp).Error)
	return topUp
}

func reloadTopUpByTradeNo(t *testing.T, tradeNo string) *TopUp {
	t.Helper()
	var tu TopUp
	require.NoError(t, DB.Where("trade_no = ?", tradeNo).First(&tu).Error)
	return &tu
}

// runCompleteEpayTopUpTx 把 CompleteEpayTopUpTx 包进一个 DB.Transaction，复现
// controller EpayNotify 的调用形态：调用方持有事务句柄，事务成功后才 ACK。
func runCompleteEpayTopUpTx(t *testing.T, tradeNo string, quotaToAdd int, actualPaymentMethod string) (bool, error) {
	t.Helper()
	var credited bool
	err := DB.Transaction(func(tx *gorm.DB) error {
		c, e := CompleteEpayTopUpTx(tx, tradeNo, quotaToAdd, actualPaymentMethod)
		credited = c
		return e
	})
	return credited, err
}

// ---------------------------------------------------------------------------
// 正常路径：Pending → 事务成功 → quota 增额 + topUp SUCCESS + credited=true
// ---------------------------------------------------------------------------

func TestCompleteEpayTopUpTx_PendingSuccessCreditsAndMarksSuccess(t *testing.T) {
	truncateTopUpTables(t)
	setWischoicerCapacity(t, math.MaxInt32)
	seedWischoicerUser(t, 61001, 0)
	seedTopUpRow(t, &TopUp{
		UserId:          61001,
		Amount:          10,
		Money:           10.0,
		TradeNo:         "EPAY_OK_001",
		PaymentMethod:   "alipay",
		PaymentProvider: PaymentProviderEpay,
		CreateTime:      common.GetTimestamp(),
		Status:          common.TopUpStatusPending,
	})

	quotaToAdd := int(10 * common.QuotaPerUnit)
	credited, err := runCompleteEpayTopUpTx(t, "EPAY_OK_001", quotaToAdd, "alipay")

	require.NoError(t, err)
	assert.True(t, credited)
	tu := reloadTopUpByTradeNo(t, "EPAY_OK_001")
	assert.Equal(t, common.TopUpStatusSuccess, tu.Status)
	assert.NotZero(t, tu.CompleteTime)
	assert.Equal(t, quotaToAdd, reloadUserQuota(t, 61001))
}

// ---------------------------------------------------------------------------
// 幂等：重复通知命中已 SUCCESS → credited=false，quota 不再增加（防双到账）
// ---------------------------------------------------------------------------

func TestCompleteEpayTopUpTx_AlreadySuccessIdempotentNoDoubleCredit(t *testing.T) {
	truncateTopUpTables(t)
	setWischoicerCapacity(t, math.MaxInt32)
	quotaToAdd := int(10 * common.QuotaPerUnit)
	seedWischoicerUser(t, 61002, 0)
	// 预置已到账的 SUCCESS 订单（模拟上一次通知已处理）。
	seedTopUpRow(t, &TopUp{
		UserId:          61002,
		Amount:          10,
		Money:           10.0,
		TradeNo:         "EPAY_DUP_002",
		PaymentMethod:   "alipay",
		PaymentProvider: PaymentProviderEpay,
		CreateTime:      common.GetTimestamp(),
		CompleteTime:    common.GetTimestamp(),
		Status:          common.TopUpStatusSuccess,
	})
	require.NoError(t, DB.Model(&User{}).Where("id = ?", 61002).Update("quota", quotaToAdd).Error)

	credited, err := runCompleteEpayTopUpTx(t, "EPAY_DUP_002", quotaToAdd, "alipay")

	require.NoError(t, err)
	assert.False(t, credited, "idempotent re-notify must not report credited")
	// quota 未二次增加（无双到账）。
	assert.Equal(t, quotaToAdd, reloadUserQuota(t, 61002))
	assert.Equal(t, common.TopUpStatusSuccess, reloadTopUpByTradeNo(t, "EPAY_DUP_002").Status)
}

// ---------------------------------------------------------------------------
// quota 增额溢出 int32 硬界 → 事务回滚 → topUp 保持 Pending + quota 不变 + err
// controller 据此不 ACK success，让 epay 重试（r7 P1-2 核心不变量）。
// ---------------------------------------------------------------------------

func TestCompleteEpayTopUpTx_QuotaOverflowRollsBackKeepsPending(t *testing.T) {
	truncateTopUpTables(t)
	// 软上限很小，迫使 CreditPaidTopUpTx 进入降级直写路径；current(2e9)+delta(5e8)
	// = 2.5e9 > MaxInt32 触发物理硬界溢出。
	setWischoicerCapacity(t, 1_000_000)
	seedWischoicerUser(t, 61003, 2_000_000_000)
	seedTopUpRow(t, &TopUp{
		UserId:          61003,
		Amount:          10,
		Money:           10.0,
		TradeNo:         "EPAY_OVF_003",
		PaymentMethod:   "alipay",
		PaymentProvider: PaymentProviderEpay,
		CreateTime:      common.GetTimestamp(),
		Status:          common.TopUpStatusPending,
	})

	credited, err := runCompleteEpayTopUpTx(t, "EPAY_OVF_003", 500_000_000, "alipay")

	require.Error(t, err)
	assert.False(t, credited)
	assert.ErrorIs(t, err, ErrWischoicerQuotaOverflow)
	// 事务回滚：订单状态与 quota 都不变（Save 已被回滚，不会出现"已 SUCCESS 但未到账"）。
	assert.Equal(t, common.TopUpStatusPending, reloadTopUpByTradeNo(t, "EPAY_OVF_003").Status)
	assert.Equal(t, 2_000_000_000, reloadUserQuota(t, 61003))
}

// ---------------------------------------------------------------------------
// provider 不匹配 → ErrPaymentMethodMismatch，quota 不变
// ---------------------------------------------------------------------------

func TestCompleteEpayTopUpTx_ProviderMismatchReturnsError(t *testing.T) {
	truncateTopUpTables(t)
	setWischoicerCapacity(t, math.MaxInt32)
	seedWischoicerUser(t, 61004, 0)
	seedTopUpRow(t, &TopUp{
		UserId:          61004,
		Amount:          10,
		Money:           10.0,
		TradeNo:         "EPAY_MM_004",
		PaymentMethod:   "alipay",
		PaymentProvider: PaymentProviderStripe,
		CreateTime:      common.GetTimestamp(),
		Status:          common.TopUpStatusPending,
	})

	credited, err := runCompleteEpayTopUpTx(t, "EPAY_MM_004", int(10*common.QuotaPerUnit), "alipay")

	require.Error(t, err)
	assert.False(t, credited)
	assert.ErrorIs(t, err, ErrPaymentMethodMismatch)
	assert.Equal(t, 0, reloadUserQuota(t, 61004))
}

// ---------------------------------------------------------------------------
// status 非 Pending 非 Success → err，订单状态与 quota 不变
// ---------------------------------------------------------------------------

func TestCompleteEpayTopUpTx_StatusInvalidReturnsError(t *testing.T) {
	truncateTopUpTables(t)
	setWischoicerCapacity(t, math.MaxInt32)
	seedWischoicerUser(t, 61005, 0)
	seedTopUpRow(t, &TopUp{
		UserId:          61005,
		Amount:          10,
		Money:           10.0,
		TradeNo:         "EPAY_ST_005",
		PaymentMethod:   "alipay",
		PaymentProvider: PaymentProviderEpay,
		CreateTime:      common.GetTimestamp(),
		Status:          common.TopUpStatusExpired,
	})

	credited, err := runCompleteEpayTopUpTx(t, "EPAY_ST_005", int(10*common.QuotaPerUnit), "alipay")

	require.Error(t, err)
	assert.False(t, credited)
	assert.Equal(t, common.TopUpStatusExpired, reloadTopUpByTradeNo(t, "EPAY_ST_005").Status)
	assert.Equal(t, 0, reloadUserQuota(t, 61005))
}

// ---------------------------------------------------------------------------
// 订单不存在 → err
// ---------------------------------------------------------------------------

func TestCompleteEpayTopUpTx_NotFoundReturnsError(t *testing.T) {
	truncateTopUpTables(t)
	setWischoicerCapacity(t, math.MaxInt32)

	credited, err := runCompleteEpayTopUpTx(t, "EPAY_MISSING_006", int(10*common.QuotaPerUnit), "alipay")

	require.Error(t, err)
	assert.False(t, credited)
}

// ---------------------------------------------------------------------------
// quotaToAdd <= 0 → err，订单保持 Pending
// ---------------------------------------------------------------------------

func TestCompleteEpayTopUpTx_InvalidQuotaReturnsError(t *testing.T) {
	truncateTopUpTables(t)
	setWischoicerCapacity(t, math.MaxInt32)
	seedWischoicerUser(t, 61007, 0)
	seedTopUpRow(t, &TopUp{
		UserId:          61007,
		Amount:          10,
		Money:           10.0,
		TradeNo:         "EPAY_QI_007",
		PaymentMethod:   "alipay",
		PaymentProvider: PaymentProviderEpay,
		CreateTime:      common.GetTimestamp(),
		Status:          common.TopUpStatusPending,
	})

	credited, err := runCompleteEpayTopUpTx(t, "EPAY_QI_007", 0, "alipay")

	require.Error(t, err)
	assert.False(t, credited)
	assert.Equal(t, common.TopUpStatusPending, reloadTopUpByTradeNo(t, "EPAY_QI_007").Status)
}

// ---------------------------------------------------------------------------
// 并发：两个 goroutine 同时处理同一 tradeNo → 恰好一个 credited=true，quota 只增一次
// ---------------------------------------------------------------------------

func TestCompleteEpayTopUpTx_ConcurrentSingleCredit(t *testing.T) {
	truncateTopUpTables(t)
	setWischoicerCapacity(t, math.MaxInt32)
	seedWischoicerUser(t, 61008, 0)
	seedTopUpRow(t, &TopUp{
		UserId:          61008,
		Amount:          10,
		Money:           10.0,
		TradeNo:         "EPAY_CONC_008",
		PaymentMethod:   "alipay",
		PaymentProvider: PaymentProviderEpay,
		CreateTime:      common.GetTimestamp(),
		Status:          common.TopUpStatusPending,
	})
	quotaToAdd := int(10 * common.QuotaPerUnit)

	const goroutines = 2
	var wg sync.WaitGroup
	var mu sync.Mutex
	creditedCount := 0
	idempotentCount := 0
	errCount := 0
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			credited, err := runCompleteEpayTopUpTx(t, "EPAY_CONC_008", quotaToAdd, "alipay")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errCount++
				return
			}
			if credited {
				creditedCount++
			} else {
				idempotentCount++
			}
		}()
	}
	wg.Wait()

	// SQLite SetMaxOpenConns(1) 串行化两个事务：第一个 credited=true，第二个幂等 credited=false。
	// MySQL/PostgreSQL 下由 lockForUpdate 行锁串行化，结论相同——跨进程不会双到账。
	assert.Equal(t, 1, creditedCount, "exactly one goroutine should credit")
	assert.Equal(t, 1, idempotentCount, "the other must hit idempotent")
	assert.Equal(t, 0, errCount, "no hard error expected under serialized contention")
	assert.Equal(t, quotaToAdd, reloadUserQuota(t, 61008), "quota must increase exactly once")
}

// ---------------------------------------------------------------------------
// 回调实际支付方式与订单不同 → 同步 PaymentMethod 字段（仅记录，不影响资金）
// ---------------------------------------------------------------------------

func TestCompleteEpayTopUpTx_ActualPaymentMethodSynced(t *testing.T) {
	truncateTopUpTables(t)
	setWischoicerCapacity(t, math.MaxInt32)
	seedWischoicerUser(t, 61009, 0)
	seedTopUpRow(t, &TopUp{
		UserId:          61009,
		Amount:          10,
		Money:           10.0,
		TradeNo:         "EPAY_PM_009",
		PaymentMethod:   "wxpay",
		PaymentProvider: PaymentProviderEpay,
		CreateTime:      common.GetTimestamp(),
		Status:          common.TopUpStatusPending,
	})

	credited, err := runCompleteEpayTopUpTx(t, "EPAY_PM_009", int(10*common.QuotaPerUnit), "alipay")

	require.NoError(t, err)
	assert.True(t, credited)
	tu := reloadTopUpByTradeNo(t, "EPAY_PM_009")
	assert.Equal(t, "alipay", tu.PaymentMethod, "actual payment method should be synced to order record")
}
