package model

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// wischoicerTestMain 在 task_cas_test.go 中已 setup in-memory SQLite DB。
// 这里补建 WischoicerRechargeCredit 表 + 设置容量上限。

func seedWischoicerUser(t *testing.T, id int, quota int) *User {
	t.Helper()
	u := &User{
		Id:       id,
		Username: fmt.Sprintf("wis_user_%d", id),
		Role:     common.RoleCommonUser,
		Status:   common.UserStatusEnabled,
		Quota:    quota,
		AffCode:  fmt.Sprintf("AFF%d", id),
	}
	require.NoError(t, DB.Create(u).Error)
	return u
}

func seedWischoicerReservedCredit(t *testing.T, c *WischoicerRechargeCredit) {
	t.Helper()
	require.NoError(t, DB.Create(c).Error)
}

func reloadCredit(t *testing.T, orderNo string) WischoicerRechargeCredit {
	t.Helper()
	var c WischoicerRechargeCredit
	require.NoError(t, DB.Where("order_no = ?", orderNo).First(&c).Error)
	return c
}

func reloadUserQuota(t *testing.T, id int) int {
	t.Helper()
	var u User
	require.NoError(t, DB.Select("quota").Where("id = ?", id).First(&u).Error)
	return u.Quota
}

func setWischoicerCapacity(t *testing.T, limit int) {
	t.Helper()
	original := common.WischoicerMaxUserQuota
	common.WischoicerMaxUserQuota = limit
	t.Cleanup(func() {
		common.WischoicerMaxUserQuota = original
	})
}

// ---------------------------------------------------------------------------
// ReserveExternalRecharge
// ---------------------------------------------------------------------------

func TestReserveExternalRecharge_FirstReservationSucceeds(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 1000000)
	seedWischoicerUser(t, 50001, 0)

	result, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_FIRST_001",
		NewApiUserId:    50001,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)
	assert.True(t, result.Reserved)
	assert.False(t, result.Duplicate)

	c := reloadCredit(t, "ORDER_FIRST_001")
	assert.Equal(t, WischoicerCreditStatusReserved, c.Status)
}

func TestReserveExternalRecharge_DuplicateSameFieldsReturnsDuplicate(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 1000000)
	seedWischoicerUser(t, 50002, 0)

	req := ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_DUP_002",
		NewApiUserId:    50002,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	}
	_, err := ReserveExternalRecharge(nil, req)
	require.NoError(t, err)

	result, err := ReserveExternalRecharge(nil, req)
	require.NoError(t, err)
	assert.True(t, result.Reserved)
	assert.True(t, result.Duplicate)

	// 容量只占用一次：无新行。
	var count int64
	require.NoError(t, DB.Model(&WischoicerRechargeCredit{}).Where("order_no = ?", req.OrderNo).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

func TestReserveExternalRecharge_DifferentFieldsReturnsConflict(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 1000000)
	seedWischoicerUser(t, 50003, 0)

	req := ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_CONFLICT_003",
		NewApiUserId:    50003,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	}
	_, err := ReserveExternalRecharge(nil, req)
	require.NoError(t, err)

	// 不同 quota → conflict。
	req.Quota = 999999
	_, err = ReserveExternalRecharge(nil, req)
	assert.ErrorIs(t, err, ErrWischoicerReservationConflict)
}

func TestReserveExternalRecharge_CapacityExceededRejected(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 800000)
	seedWischoicerUser(t, 50004, 400000)

	// current(400000) + delta(500000) = 900000 > 800000。
	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_CAP_004",
		NewApiUserId:    50004,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	assert.ErrorIs(t, err, ErrWischoicerQuotaCapacityExceeded)

	// 没有创建凭据。
	var count int64
	require.NoError(t, DB.Model(&WischoicerRechargeCredit{}).Where("order_no = ?", "ORDER_CAP_004").Count(&count).Error)
	assert.EqualValues(t, 0, count)
}

func TestReserveExternalRecharge_BoundaryEqualsLimitSucceeds(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 1000000)
	seedWischoicerUser(t, 50005, 300000)

	// current(300000) + delta(700000) = 1000000 == limit → OK。
	result, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_BOUNDARY_005",
		NewApiUserId:    50005,
		Quota:           700000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)
	assert.True(t, result.Reserved)
}

func TestReserveExternalRecharge_ConcurrentSameOrderOnlyOneReservation(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 5000000)
	seedWischoicerUser(t, 50006, 0)

	req := ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_CONCURRENT_006",
		NewApiUserId:    50006,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	}

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	successCount := 0
	dupCount := 0
	errCount := 0
	var mu sync.Mutex

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			r, err := ReserveExternalRecharge(nil, req)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errCount++
				return
			}
			if r.Duplicate {
				dupCount++
			} else {
				successCount++
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, successCount, "exactly one goroutine should create the reservation")
	assert.Equal(t, goroutines-1, dupCount+errCount, "others should see duplicate or error")
	assert.Equal(t, 0, errCount, "no unexpected errors")

	// 容量只占用一次：只有一行。
	var count int64
	require.NoError(t, DB.Model(&WischoicerRechargeCredit{}).Where("order_no = ?", req.OrderNo).Count(&count).Error)
	assert.EqualValues(t, 1, count)

	// 汇总占用 = quota * 1。
	sum, err := sumActiveReservedQuotaTx(DB.Session(&gorm.Session{}), 50006)
	require.NoError(t, err)
	assert.Equal(t, 500000, sum)
}

// ---------------------------------------------------------------------------
// CreditUserQuotaTx / CreditUserQuota — 容量守卫
// ---------------------------------------------------------------------------

func TestCreditUserQuotaTx_RespectsCapacityLimit(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 1000000)
	seedWischoicerUser(t, 50010, 600000)

	// 预留一条占用 300000。
	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_GUARD_010",
		NewApiUserId:    50010,
		Quota:           300000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	// current(600000) + reserved(300000) + delta(200000) = 1100000 > 1000000。
	err = DB.Transaction(func(tx *gorm.DB) error {
		return CreditUserQuotaTx(nil, tx, 50010, 200000)
	})
	assert.ErrorIs(t, err, ErrWischoicerQuotaCapacityExceeded)

	// current(600000) + reserved(300000) + delta(100000) = 1000000 == limit → OK。
	err = DB.Transaction(func(tx *gorm.DB) error {
		return CreditUserQuotaTx(nil, tx, 50010, 100000)
	})
	require.NoError(t, err)
	assert.Equal(t, 700000, reloadUserQuota(t, 50010))
}

func TestCreditUserQuota_NonTxWrapperRejectsNonPositive(t *testing.T) {
	truncateWischoicerTables(t)
	assert.ErrorIs(t, CreditUserQuota(1, 0), ErrWischoicerInvalidArgument)
	assert.ErrorIs(t, CreditUserQuota(1, -5), ErrWischoicerInvalidArgument)
}

// ---------------------------------------------------------------------------
// ConsumeReservedQuotaTx / CreditExternalRecharge
// ---------------------------------------------------------------------------

func TestCreditExternalRecharge_FirstCreditConsumesReservation(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 5000000)
	seedWischoicerUser(t, 50020, 100000)

	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_CREDIT_020",
		NewApiUserId:    50020,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	result, err := CreditExternalRecharge(nil, CreditExternalRechargeRequest{
		OrderNo:         "ORDER_CREDIT_020",
		NewApiUserId:    50020,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
		TransactionId:   "TX_4200000020260710000000000020",
		PaidAt:          1720000000,
	})
	require.NoError(t, err)
	assert.True(t, result.Credited)
	assert.False(t, result.Duplicate)

	// quota 增加 + 凭据 SUCCESS。
	assert.Equal(t, 600000, reloadUserQuota(t, 50020))
	c := reloadCredit(t, "ORDER_CREDIT_020")
	assert.Equal(t, WischoicerCreditStatusSuccess, c.Status)
	require.NotNil(t, c.ExternalTransactionId)
	assert.Equal(t, "TX_4200000020260710000000000020", *c.ExternalTransactionId)
	assert.EqualValues(t, 1720000000, c.PaidTime)
}

func TestCreditExternalRecharge_DuplicateSameFieldsNoDoubleCredit(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 5000000)
	seedWischoicerUser(t, 50021, 0)

	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_CREDIT_021",
		NewApiUserId:    50021,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	req := CreditExternalRechargeRequest{
		OrderNo:         "ORDER_CREDIT_021",
		NewApiUserId:    50021,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
		TransactionId:   "TX_021",
		PaidAt:          1720000001,
	}
	r1, err := CreditExternalRecharge(nil, req)
	require.NoError(t, err)
	assert.True(t, r1.Credited)
	assert.Equal(t, 500000, reloadUserQuota(t, 50021))

	// 重试（响应丢失场景）→ duplicate，quota 不变。
	r2, err := CreditExternalRecharge(nil, req)
	require.NoError(t, err)
	assert.False(t, r2.Credited)
	assert.True(t, r2.Duplicate)
	assert.Equal(t, 500000, reloadUserQuota(t, 50021))
}

func TestCreditExternalRecharge_FieldConflictReturnsCreditConflict(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 5000000)
	seedWischoicerUser(t, 50022, 0)

	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_CREDIT_022",
		NewApiUserId:    50022,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	req := CreditExternalRechargeRequest{
		OrderNo:         "ORDER_CREDIT_022",
		NewApiUserId:    50022,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
		TransactionId:   "TX_022",
		PaidAt:          1720000002,
	}
	_, err = CreditExternalRecharge(nil, req)
	require.NoError(t, err)

	// 相同 orderNo 不同 transactionId → CREDIT_CONFLICT，quota 不变。
	req.TransactionId = "TX_022_DIFFERENT"
	_, err = CreditExternalRecharge(nil, req)
	assert.ErrorIs(t, err, ErrWischoicerCreditConflict)
	assert.Equal(t, 500000, reloadUserQuota(t, 50022))
}

func TestCreditExternalRecharge_DuplicateTransactionIdRejected(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 5000000)
	seedWischoicerUser(t, 50023, 0)
	seedWischoicerUser(t, 50024, 0)

	// 订单 A 入账，transactionId = SHARED_TX。
	for _, uid := range []int{50023} {
		_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
			OrderNo:         fmt.Sprintf("ORDER_TX_A_%d", uid),
			NewApiUserId:    uid,
			Quota:           500000,
			AmountCents:     1000,
			Currency:        "CNY",
			PaymentProvider: "wischoicer_wechat",
		})
		require.NoError(t, err)
	}
	_, err := CreditExternalRecharge(nil, CreditExternalRechargeRequest{
		OrderNo:         "ORDER_TX_A_50023",
		NewApiUserId:    50023,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
		TransactionId:   "SHARED_TX",
		PaidAt:          1720000003,
	})
	require.NoError(t, err)

	// 订单 B 用相同 transactionId 不同 orderNo → 拒绝。
	_, err = ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_TX_B_50024",
		NewApiUserId:    50024,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	_, err = CreditExternalRecharge(nil, CreditExternalRechargeRequest{
		OrderNo:         "ORDER_TX_B_50024",
		NewApiUserId:    50024,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
		TransactionId:   "SHARED_TX",
		PaidAt:          1720000003,
	})
	assert.ErrorIs(t, err, ErrWischoicerCreditConflict)
	assert.Equal(t, 0, reloadUserQuota(t, 50024))
}

func TestCreditExternalRecharge_ReleasedReservationRejected(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 5000000)
	seedWischoicerUser(t, 50025, 0)

	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_CREDIT_025",
		NewApiUserId:    50025,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	require.NoError(t, ReleaseExternalRecharge(nil, "ORDER_CREDIT_025", "order_closed"))

	_, err = CreditExternalRecharge(nil, CreditExternalRechargeRequest{
		OrderNo:         "ORDER_CREDIT_025",
		NewApiUserId:    50025,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
		TransactionId:   "TX_025",
		PaidAt:          1720000004,
	})
	assert.ErrorIs(t, err, ErrWischoicerReservationReleased)
	assert.Equal(t, 0, reloadUserQuota(t, 50025))
}

func TestCreditExternalRecharge_UserNotFoundRejected(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 5000000)

	// 无该用户的预留凭据：先找不到用户 → CREDIT_USER_UNAVAILABLE。
	_, err := CreditExternalRecharge(nil, CreditExternalRechargeRequest{
		OrderNo:         "ORDER_CREDIT_NOUSER",
		NewApiUserId:    99999,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
		TransactionId:   "TX_NOUSER",
		PaidAt:          1720000005,
	})
	assert.ErrorIs(t, err, ErrWischoicerCreditUserUnavailable)
}

// ---------------------------------------------------------------------------
// ReleaseExternalRecharge
// ---------------------------------------------------------------------------

func TestReleaseExternalRecharge_IdempotentAfterReleased(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 5000000)
	seedWischoicerUser(t, 50030, 0)

	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_RELEASE_030",
		NewApiUserId:    50030,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	require.NoError(t, ReleaseExternalRecharge(nil, "ORDER_RELEASE_030", "closed"))
	// 再次 release 幂等。
	require.NoError(t, ReleaseExternalRecharge(nil, "ORDER_RELEASE_030", "closed"))

	c := reloadCredit(t, "ORDER_RELEASE_030")
	assert.Equal(t, WischoicerCreditStatusReleased, c.Status)
	assert.Equal(t, "closed", c.ReleaseReason)
}

func TestReleaseExternalRecharge_SuccessCreditCannotRelease(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 5000000)
	seedWischoicerUser(t, 50031, 0)

	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_RELEASE_031",
		NewApiUserId:    50031,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	_, err = CreditExternalRecharge(nil, CreditExternalRechargeRequest{
		OrderNo:         "ORDER_RELEASE_031",
		NewApiUserId:    50031,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
		TransactionId:   "TX_031",
		PaidAt:          1720000006,
	})
	require.NoError(t, err)

	err = ReleaseExternalRecharge(nil, "ORDER_RELEASE_031", "should_fail")
	assert.ErrorIs(t, err, ErrWischoicerReservationConflict)
	c := reloadCredit(t, "ORDER_RELEASE_031")
	assert.Equal(t, WischoicerCreditStatusSuccess, c.Status)
}

// ---------------------------------------------------------------------------
// 用户删除保护
// ---------------------------------------------------------------------------

func TestDeleteUserBlockedWhenReservationExists(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 5000000)
	seedWischoicerUser(t, 50040, 0)

	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_DEL_040",
		NewApiUserId:    50040,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	err = DeleteUserById(50040)
	assert.Error(t, err)

	err = HardDeleteUserById(50040)
	assert.Error(t, err)

	// release 后可软删除。
	require.NoError(t, ReleaseExternalRecharge(nil, "ORDER_DEL_040", "closed"))
	require.NoError(t, DeleteUserById(50040))
}

// ---------------------------------------------------------------------------
// 迁移后的正向额度路径不能侵占预留容量
// ---------------------------------------------------------------------------

func TestCreditUserQuotaTx_RedeemStylePathBlockedByReservation(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 1000000)
	seedWischoicerUser(t, 50050, 600000)

	// 预留 300000 容量。
	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_MIGRATE_050",
		NewApiUserId:    50050,
		Quota:           300000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	// 模拟 topup/redemption/checkin 迁移后的路径：current(600000)+reserved(300000)+delta(200000)=1100000>limit。
	err = CreditUserQuota(50050, 200000)
	assert.ErrorIs(t, err, ErrWischoicerQuotaCapacityExceeded)
	assert.Equal(t, 600000, reloadUserQuota(t, 50050))

	// delta=100000 时 == limit → OK。
	require.NoError(t, CreditUserQuota(50050, 100000))
	assert.Equal(t, 700000, reloadUserQuota(t, 50050))
}

// 消费 RESERVED 凭据时不检查容量：该凭据的 quota 已在 Reserve 阶段计入
// activeReservedQuota，消费只是把 reserved 转为 actual、净额不变。即使
// user.quota + delta > limit（例如退款降级直写把 current 推过 limit），已付款的
// RESERVED 消费仍必须成功，否则用户付了钱到不了账。
func TestConsumeReservedQuotaTx_SucceedsWhenCurrentExceedsLimit(t *testing.T) {
	truncateWischoicerTables(t)
	// 故意把 limit 设到 < 用户 quota + reserved quota，模拟 current 已被退款突破。
	// user.quota=900000, reserved quota=500000 → consume 后 1400000 > limit 1000000。
	setWischoicerCapacity(t, 1000000)
	seedWischoicerUser(t, 50060, 900000)

	// 直接插入 RESERVED 凭据（绕过 reserve 的容量校验，模拟退款把 current 推过 limit
	// 后仍残留的已付款预留）。
	seedWischoicerReservedCredit(t, &WischoicerRechargeCredit{
		OrderNo:         "ORDER_ROLLBACK_060",
		NewAPIUserId:    50060,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
		Status:          WischoicerCreditStatusReserved,
		CacheStatus:     WischoicerCacheStatusPending,
	})

	_, err := CreditExternalRecharge(nil, CreditExternalRechargeRequest{
		OrderNo:         "ORDER_ROLLBACK_060",
		NewApiUserId:    50060,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
		TransactionId:   "TX_060",
		PaidAt:          1720000007,
	})
	require.NoError(t, err)

	// 消费成功：quota 增加（即便已超 limit），凭据转 SUCCESS。
	assert.Equal(t, 1400000, reloadUserQuota(t, 50060))
	c := reloadCredit(t, "ORDER_ROLLBACK_060")
	assert.Equal(t, WischoicerCreditStatusSuccess, c.Status)
	require.NotNil(t, c.ExternalTransactionId)
	assert.Equal(t, "TX_060", *c.ExternalTransactionId)
}

// 关键回归：RefundUserQuota 降级直写把 user.quota 推过软上限后，已付款 RESERVED 凭据
// 的消费仍能成功。软上限（WISCHOICER_MAX_USER_QUOTA）只是「新预约门槛」，不是物理硬界；
// consumeQuotaForCreditTx 不再检查它，避免退款 fallback 永久阻断已付款 reservation 消费。
// 同时验证：退款降级产生 SysError 审计；新 reservation 仍被软上限守卫正确拒绝。
func TestConsumeReservedQuota_SucceedsAfterRefundBreakthrough(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 1000000)
	// 初始 current=400000，预留 500000（current+reserved=900000 <= limit）。
	seedWischoicerUser(t, 50061, 400000)
	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_REFUND_THEN_CONSUME",
		NewApiUserId:    50061,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	// 退款 700000：软上限守卫拒绝（400000 + 500000 + 700000 = 1600000 > limit），降级直写，
	// quota → 1100000 > 软上限。退款必须到账（软上限不是硬界），但降级必须产生 SysError 审计。
	logBuf := captureSysError(t)
	require.NoError(t, RefundUserQuota(50061, 700000))
	require.Equal(t, 1100000, reloadUserQuota(t, 50061))
	assert.Contains(t, logBuf.String(), "falling back to direct increase")

	// 已付款的 RESERVED 凭据仍能成功消费（不被 consumeQuotaForCreditTx 拒绝）。
	result, err := CreditExternalRecharge(nil, CreditExternalRechargeRequest{
		OrderNo:         "ORDER_REFUND_THEN_CONSUME",
		NewApiUserId:    50061,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
		TransactionId:   "TX_REFUND_THEN_CONSUME",
		PaidAt:          1720000009,
	})
	require.NoError(t, err)
	require.True(t, result.Credited)
	// quota = 1100000 + 500000 = 1600000。
	assert.Equal(t, 1600000, reloadUserQuota(t, 50061))
	c := reloadCredit(t, "ORDER_REFUND_THEN_CONSUME")
	assert.Equal(t, WischoicerCreditStatusSuccess, c.Status)

	// 新 reservation 被正确拒绝：current(1600000) + reserved(0) + new(1) > limit。
	_, err = ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_REFUND_THEN_BLOCK",
		NewApiUserId:    50061,
		Quota:           1,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	assert.ErrorIs(t, err, ErrWischoicerQuotaCapacityExceeded)
}

func TestConsumeReservedQuotaTx_ConcurrentOnlyOneCredit(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 50000000)
	seedWischoicerUser(t, 50070, 0)

	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_CONCURRENT_070",
		NewApiUserId:    50070,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	req := CreditExternalRechargeRequest{
		OrderNo:         "ORDER_CONCURRENT_070",
		NewApiUserId:    50070,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
		TransactionId:   "TX_070",
		PaidAt:          1720000008,
	}

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	credited := 0
	duplicated := 0
	failed := 0
	var mu sync.Mutex

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			r, err := CreditExternalRecharge(nil, req)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				return
			}
			if r.Credited {
				credited++
			} else if r.Duplicate {
				duplicated++
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, credited, "exactly one credit")
	assert.Equal(t, goroutines-1, duplicated, "rest are duplicates")
	assert.Equal(t, 0, failed, "no unexpected failures")
	assert.Equal(t, 500000, reloadUserQuota(t, 50070))
}

// ---------------------------------------------------------------------------
// nullable unique transaction id：多条 NULL 允许
// ---------------------------------------------------------------------------

func TestWischoicerRechargeCredit_MultipleNullTransactionIdAllowed(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 5000000)

	// 两条 RESERVED 凭据都没有 transaction_id（NULL），应同时存在。
	seedWischoicerReservedCredit(t, &WischoicerRechargeCredit{
		OrderNo:         "ORDER_NULL_A",
		NewAPIUserId:    1,
		Quota:           100,
		AmountCents:     1,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
		Status:          WischoicerCreditStatusReserved,
	})
	seedWischoicerReservedCredit(t, &WischoicerRechargeCredit{
		OrderNo:         "ORDER_NULL_B",
		NewAPIUserId:    1,
		Quota:           100,
		AmountCents:     1,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
		Status:          WischoicerCreditStatusReserved,
	})

	var count int64
	require.NoError(t, DB.Model(&WischoicerRechargeCredit{}).Where("external_transaction_id IS NULL").Count(&count).Error)
	assert.EqualValues(t, 2, count)
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func truncateWischoicerTables(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		DB.Exec("DELETE FROM wischoicer_recharge_credits")
		DB.Exec("DELETE FROM users")
	})
}

// captureSysError 捕获 common.SysError 写入 gin.DefaultErrorWriter 的输出，供测试断言
// 降级路径/溢出告警被正确审计。SysError 读取 writer 时持 LogWriterMu.RLock，替换/恢复
// 用 Lock 同步。测试串行运行，不存在并发干扰。
func captureSysError(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	common.LogWriterMu.Lock()
	orig := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = buf
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = orig
		common.LogWriterMu.Unlock()
	})
	return buf
}

// ---------------------------------------------------------------------------
// P1-3：IncreaseUserQuota 正向增加路径必须经过容量守卫
// ---------------------------------------------------------------------------

// db=true 路径在 IncreaseUserQuota 内部委托 CreditUserQuota 守卫；
// current + reserved + delta > limit 时拒绝且不改变 quota，delta 使总和恰好等于 limit 时放行。
func TestIncreaseUserQuota_DbTrueBlockedByCapacityGuard(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 1000000)
	seedWischoicerUser(t, 50080, 600000)

	// 预留 300000：current(600000)+reserved(300000)=900000，剩余 100000。
	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_GUARD_080",
		NewApiUserId:    50080,
		Quota:           300000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	// delta=200000 使总和 1100000 > limit → 守卫拒绝。
	err = IncreaseUserQuota(50080, 200000, true)
	assert.ErrorIs(t, err, ErrWischoicerQuotaCapacityExceeded)
	assert.Equal(t, 600000, reloadUserQuota(t, 50080))

	// delta=100000 使总和 == limit → 成功。
	require.NoError(t, IncreaseUserQuota(50080, 100000, true))
	assert.Equal(t, 700000, reloadUserQuota(t, 50080))
}

// ---------------------------------------------------------------------------
// P1-2：RefundUserQuota 退还先前扣除，走守卫并在容量瞬时打满时降级直写
// ---------------------------------------------------------------------------

// 守卫通过时 RefundUserQuota 等价于 CreditUserQuota：current + reserved + delta <= limit 放行。
func TestRefundUserQuota_GuardAdmitsWhenCapacityAvailable(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 1000000)
	seedWischoicerUser(t, 50081, 600000)

	// 预留 300000：current(600000)+reserved(300000)=900000，剩余 100000。
	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_REFUND_081",
		NewApiUserId:    50081,
		Quota:           300000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	// 退还 100000 使总和恰好 == limit → 守卫放行，quota 到账。
	require.NoError(t, RefundUserQuota(50081, 100000))
	assert.Equal(t, 700000, reloadUserQuota(t, 50081))
}

// 守卫拒绝时（current + reserved + delta > limit）RefundUserQuota 必须降级直写，
// 保证退款不丢失额度；quota 实际到账，不变量瞬时被突破但无资金损失。
func TestRefundUserQuota_FallbackOnCapacityGuardReject(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 1000000)
	// current(850000) + reserved(100000) = 950000；退款 100000 → 1050000 > limit。
	seedWischoicerUser(t, 50082, 850000)
	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_REFUND_082",
		NewApiUserId:    50082,
		Quota:           100000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	// 退还 100000：软上限守卫拒绝但降级直写，quota 必须到账，并产生 SysError 审计。
	logBuf := captureSysError(t)
	require.NoError(t, RefundUserQuota(50082, 100000))
	assert.Equal(t, 950000, reloadUserQuota(t, 50082))
	assert.Contains(t, logBuf.String(), "falling back to direct increase")

	// 后续 Reserve 在容量检查时安全失败：current(950000)+reserved(100000)+new(1) > limit。
	_, err = ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_REFUND_082_BLOCK",
		NewApiUserId:    50082,
		Quota:           1,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.ErrorIs(t, err, ErrWischoicerQuotaCapacityExceeded)
}

// 无 RESERVED 时 RefundUserQuota 不会触发降级：守卫必定放行（current + 0 + delta <= limit）。
func TestRefundUserQuota_NoReservationAlwaysAdmitted(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 1000000)
	seedWischoicerUser(t, 50083, 400000)

	require.NoError(t, RefundUserQuota(50083, 500000))
	assert.Equal(t, 900000, reloadUserQuota(t, 50083))
}

// 非正 delta 是 no-op，不会触碰守卫或写库。
func TestRefundUserQuota_NonPositiveDeltaIsNoOp(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 1000000)
	seedWischoicerUser(t, 50084, 300000)

	require.NoError(t, RefundUserQuota(50084, 0))
	require.NoError(t, RefundUserQuota(50084, -5))
	assert.Equal(t, 300000, reloadUserQuota(t, 50084))
}

// 退款叠加会溢出 int32 物理硬界时，RefundUserQuota 拒绝直写、不改变 quota、SysError 告警。
// 这是「退款必须到账」的唯一例外——int32 物理溢出无业务解，需运维人工介入。
// 软上限（WISCHOICER_MAX_USER_QUOTA）放到 MaxInt32，确保退款先被软上限守卫拒绝再走降级，
// 由降级路径的 int32 CAS 兜住。
func TestRefundUserQuota_RejectedWhenInt32Overflow(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, math.MaxInt32)
	// current 接近 MaxInt32；叠加 delta 后溢出 int32 物理硬界。
	seedWischoicerUser(t, 50085, math.MaxInt32-100)

	logBuf := captureSysError(t)
	err := RefundUserQuota(50085, 200)
	require.ErrorIs(t, err, ErrWischoicerQuotaOverflow)
	// CAS 拒绝写入，quota 保持不变。
	assert.Equal(t, math.MaxInt32-100, reloadUserQuota(t, 50085))
	// 告警包含溢出关键字，供运维检索。
	assert.Contains(t, logBuf.String(), "overflow int32 hard cap")
}

// R5 P1 回归：refundUserQuotaDirectWithInt32Cap 的硬界检查必须覆盖
// activeReservedQuota，不能只看 current。反例：current=MaxInt32-150，
// activeReserved=100（Reserve 阶段合法：current+reserved+new<=MaxInt32），
// refund=100。修复前的 CAS 只查 current+delta<=MaxInt32
// （MaxInt32-150+100=MaxInt32-50，合法通过），退款成功后 current=MaxInt32-50；
// 后续消费该 100 的 RESERVED 凭据时 consumeQuotaForCreditTx 直接
// current+reservedDelta=MaxInt32-50+100=MaxInt32+50，物理溢出，
// 已付款订单永久死信。修复后退款阶段必须正确拒绝（覆盖 reserved）。
func TestRefundUserQuota_RejectedWhenReservedPlusCurrentWouldOverflow(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, math.MaxInt32)
	seedWischoicerUser(t, 50086, math.MaxInt32-150)

	// Reserve 阶段合法：current(MaxInt32-150) + reserved(0) + new(100) <= MaxInt32。
	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_OVERFLOW_086",
		NewApiUserId:    50086,
		Quota:           100,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	// 退款 100：current+delta=MaxInt32-50 本身不溢出，但 current+reserved+delta
	// =MaxInt32-150+100+100=MaxInt32+50 会溢出，必须被拒绝。
	logBuf := captureSysError(t)
	err = RefundUserQuota(50086, 100)
	require.ErrorIs(t, err, ErrWischoicerQuotaOverflow)
	assert.Equal(t, math.MaxInt32-150, reloadUserQuota(t, 50086))
	assert.Contains(t, logBuf.String(), "overflow int32 hard cap including active reservations")

	// 已付款的 RESERVED 凭据仍能安全消费（未被退款推向溢出）。
	result, err := CreditExternalRecharge(nil, CreditExternalRechargeRequest{
		OrderNo:         "ORDER_OVERFLOW_086",
		NewApiUserId:    50086,
		Quota:           100,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
		TransactionId:   "TX_OVERFLOW_086",
		PaidAt:          1720000010,
	})
	require.NoError(t, err)
	require.True(t, result.Credited)
	assert.Equal(t, math.MaxInt32-50, reloadUserQuota(t, 50086))
}

// R5 P1 回归：软上限拒绝触发降级，但覆盖 reserved 后仍在 int32 硬界内时正常降级到账。
func TestRefundUserQuota_FallbackSucceedsWhenReservedWithinInt32Cap(t *testing.T) {
	truncateWischoicerTables(t)
	// 软上限低于 MaxInt32，确保退款先被软上限拒绝再走降级；降级路径的硬界检查
	// （current+reserved+delta<=MaxInt32）仍应放行。
	setWischoicerCapacity(t, math.MaxInt32-800)
	seedWischoicerUser(t, 50087, math.MaxInt32-1000)

	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_OVERFLOW_087",
		NewApiUserId:    50087,
		Quota:           100,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	// 软上限：current+reserved+delta = (MaxInt32-1000)+100+200 = MaxInt32-700 > limit(MaxInt32-800) → 拒绝，降级。
	// 硬界：MaxInt32-700 <= MaxInt32 → 放行。
	logBuf := captureSysError(t)
	require.NoError(t, RefundUserQuota(50087, 200))
	assert.Equal(t, math.MaxInt32-800, reloadUserQuota(t, 50087))
	assert.Contains(t, logBuf.String(), "falling back to direct increase")
}

// R5 P1 回归：并发退款时，若两者叠加会推过硬界（覆盖 reserved），只有一个能成功，
// 验证锁 user 行 + 事务重试下 CAS/锁生效，不会出现双写导致的物理溢出。
func TestRefundUserQuota_ConcurrentOnlyOneSucceedsWhenWouldOverflow(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, math.MaxInt32)
	seedWischoicerUser(t, 50088, math.MaxInt32-150)

	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_OVERFLOW_088",
		NewApiUserId:    50088,
		Quota:           100,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	// 每次退款 60：current(MaxInt32-150)+reserved(100)+60 = MaxInt32+10，单次已溢出，
	// 两次并发都应被拒绝（每次都会溢出，不存在"仅一次安全"的情形，用于验证串行化下
	// 两次都被正确拒绝、quota 保持不变、无并发数据竞争造成的意外通过）。
	const attempts = 2
	var wg sync.WaitGroup
	wg.Add(attempts)
	errCount := 0
	var mu sync.Mutex
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()
			err := RefundUserQuota(50088, 60)
			mu.Lock()
			defer mu.Unlock()
			if errors.Is(err, ErrWischoicerQuotaOverflow) {
				errCount++
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, attempts, errCount, "both refunds should be rejected, neither may overflow int32")
	assert.Equal(t, math.MaxInt32-150, reloadUserQuota(t, 50088))
}

// ---------------------------------------------------------------------------
// P1-5：删除用户与创建预留的 TOCTOU
// ---------------------------------------------------------------------------

// DeleteUserById/HardDeleteUserById 被拦时用户不能被删除：事务内重查发现预留即回滚，
// 软删除的 deleted_at 保持 NULL，硬删除不发生物理删除。
func TestDeleteUserById_NoDeleteWhenReservationBlocks(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 5000000)
	seedWischoicerUser(t, 50090, 0)

	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_TOCTOU_090",
		NewApiUserId:    50090,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	// 软删除被拦，deleted_at 保持 NULL。
	require.Error(t, DeleteUserById(50090))
	var user User
	require.NoError(t, DB.Unscoped().Where("id = ?", 50090).First(&user).Error)
	assert.False(t, user.DeletedAt.Valid)

	// 硬删除同样被拦，行仍在。
	require.Error(t, HardDeleteUserById(50090))
	var count int64
	DB.Unscoped().Model(&User{}).Where("id = ?", 50090).Count(&count)
	assert.Equal(t, int64(1), count)
}

// TestDeleteVsReserve_NoLostReservation 并发跑删除与预留，断言不会出现
// 「用户已删除 且 存在活跃 RESERVED 预留」的丢失状态。
//
// SQLite 单写连接下退化为串行；MySQL/PostgreSQL 的 lockForUpdate(FOR UPDATE)
// 才让删除事务与 reserve 事务在 user 行上真正串行化，覆盖 TOCTOU 竞态。
// 本测试在 MySQL/PG CI 下回归保护该不变量。
func TestDeleteVsReserve_NoLostReservation(t *testing.T) {
	truncateWischoicerTables(t)
	setWischoicerCapacity(t, 5000000)
	seedWischoicerUser(t, 50091, 0)

	const deleters = 2
	const reservers = 3
	var wg sync.WaitGroup
	wg.Add(deleters + reservers)

	for i := 0; i < deleters; i++ {
		go func() {
			defer wg.Done()
			_ = DeleteUserById(50091)
		}()
	}
	for i := 0; i < reservers; i++ {
		go func(idx int) {
			defer wg.Done()
			_, _ = ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
				OrderNo:         fmt.Sprintf("ORDER_RACE_%d", idx),
				NewApiUserId:    50091,
				Quota:           100000,
				AmountCents:     1000,
				Currency:        "CNY",
				PaymentProvider: "wischoicer_wechat",
			})
		}(i)
	}
	wg.Wait()

	// 不变量：用户被（软）删除时不能残留活跃 RESERVED 预留。
	var user User
	err := DB.Where("id = ?", 50091).First(&user).Error
	userGone := errors.Is(err, gorm.ErrRecordNotFound)
	hasReserved, _ := HasActiveWischoicerReservation(50091)
	if userGone && hasReserved {
		t.Fatalf("TOCTOU: user deleted but active reservation remains")
	}
}
