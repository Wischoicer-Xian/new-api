//go:build integration

// These tests lift the wischoicer feature's money-critical invariants onto a
// real MySQL/PostgreSQL (testcontainers) so that SELECT ... FOR UPDATE, record
// locks, unique indexes, CAS and the int32 column width are actually exercised.
//
// The in-memory SQLite suite (wischoicer_recharge_test.go / topup_test.go)
// cannot reproduce any of these: SQLite has no FOR UPDATE (lockForUpdate skips
// the clause), serializes on a single write lock, and stores integers up to
// int64 regardless of the declared column type — so the 32-bit quota overflow
// that the review flagged as a silent-wrap risk is invisible there.
//
// Run with:  go test -tags integration ./model/...
// Without the build tag these tests are not compiled; the default
// `go test ./...` gate is unaffected.
//
// Every assertion is made on durable DB state (final rows/columns/counts), not
// on function return values alone — the "Coverage Illusion" fix from 第九轮
// codex review P1-3. Each test starts a fresh container via
// setupWischoicerIntegrationDB, so there is no cross-test pollution and no truncation
// helper is needed.
package model

import (
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// 1. Concurrent ReserveExternalRecharge on the same orderNo — unique index
// ---------------------------------------------------------------------------

// On MySQL the order_no unique index + the user-row FOR UPDATE lock make exactly
// one goroutine create the RESERVED credit; the rest either observe it via the
// OnConflict(DoNothing)→reread path (duplicate=true) or block on the user row
// lock and then see the existing credit. SQLite's single-writer model cannot
// prove the unique-index race or the lock serialization.
func TestReserveExternalRecharge_DatabaseConcurrentSameOrderOnlyOneReservation(t *testing.T) {
	setupWischoicerIntegrationDB(t)
	setWischoicerCapacity(t, 5000000)
	seedWischoicerUser(t, 60001, 0)

	req := ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_INT_CONCURRENT_001",
		NewApiUserId:    60001,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	}

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	successCount, dupCount, errCount := 0, 0, 0
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

	// Durable state: only one credit row; reserved sum occupies quota once.
	var rowCount int64
	require.NoError(t, DB.Model(&WischoicerRechargeCredit{}).Where("order_no = ?", req.OrderNo).Count(&rowCount).Error)
	assert.EqualValues(t, 1, rowCount, "exactly one credit row in MySQL")

	sum, err := sumActiveReservedQuotaTx(DB.Session(&gorm.Session{}), 60001)
	require.NoError(t, err)
	assert.Equal(t, 500000, sum, "reserved sum must equal one reservation's quota")
}

// ---------------------------------------------------------------------------
// 2. Concurrent reserve occupies the same user capacity — row lock prevents oversell
// ---------------------------------------------------------------------------

// Two goroutines each try to reserve more than the remaining capacity. The
// user-row FOR UPDATE lock serializes them: the first commits its reservation,
// the second re-reads the (now larger) reserved sum and is rejected with
// QUOTA_CAPACITY_EXCEEDED. On SQLite this collapses to serial execution and
// cannot prove the lock prevents oversell under real concurrency.
func TestReserveExternalRecharge_DatabaseConcurrentCapacityOversellPrevented(t *testing.T) {
	setupWischoicerIntegrationDB(t)
	// Capacity fits exactly one 600k reservation; two would oversell.
	setWischoicerCapacity(t, 1000000)
	seedWischoicerUser(t, 60002, 0)

	makeReq := func(orderNo string) ReserveExternalRechargeRequest {
		return ReserveExternalRechargeRequest{
			OrderNo:         orderNo,
			NewApiUserId:    60002,
			Quota:           600000,
			AmountCents:     1000,
			Currency:        "CNY",
			PaymentProvider: "wischoicer_wechat",
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	type outcome struct {
		reserved bool
		err      error
	}
	results := make([]outcome, 2)

	for i := 0; i < 2; i++ {
		go func(idx int) {
			defer wg.Done()
			r, err := ReserveExternalRecharge(nil, makeReq("ORDER_INT_CAP_"+string(rune('A'+idx))))
			results[idx] = outcome{reserved: err == nil && r != nil && r.Reserved, err: err}
		}(i)
	}
	wg.Wait()

	successCount := 0
	capExceeded := 0
	for _, r := range results {
		if r.reserved {
			successCount++
		} else if errors.Is(r.err, ErrWischoicerQuotaCapacityExceeded) {
			capExceeded++
		}
	}
	assert.Equal(t, 1, successCount, "exactly one reservation should fit")
	assert.Equal(t, 1, capExceeded, "the other must be rejected for capacity")

	// Durable state: only one RESERVED credit; user quota untouched (reserve
	// does not increase quota, only reserves capacity).
	var rowCount int64
	require.NoError(t, DB.Model(&WischoicerRechargeCredit{}).
		Where("new_api_user_id = ? AND status = ?", 60002, WischoicerCreditStatusReserved).
		Count(&rowCount).Error)
	assert.EqualValues(t, 1, rowCount, "only one RESERVED credit must survive")
	assert.Equal(t, 0, reloadUserQuota(t, 60002), "reserve must not touch user.quota")
}

// ---------------------------------------------------------------------------
// 3. Concurrent CreditExternalRecharge on the same RESERVED credential — CAS
// ---------------------------------------------------------------------------

// Two goroutines credit the same RESERVED credential. The credit-row FOR UPDATE
// lock + the RESERVED→SUCCESS CAS (RowsAffected==1) make exactly one win and
// increase quota; the loser observes SUCCESS and returns duplicate. MySQL's real
// row lock is what the billing→new-api credit path depends on for "quota only
// increases once".
func TestCreditExternalRecharge_DatabaseConcurrentOnlyOneCredit(t *testing.T) {
	setupWischoicerIntegrationDB(t)
	setWischoicerCapacity(t, 50000000)
	seedWischoicerUser(t, 60003, 0)

	_, err := ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
		OrderNo:         "ORDER_INT_CREDIT_003",
		NewApiUserId:    60003,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
	})
	require.NoError(t, err)

	req := CreditExternalRechargeRequest{
		OrderNo:         "ORDER_INT_CREDIT_003",
		NewApiUserId:    60003,
		Quota:           500000,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
		TransactionId:   "TX_INT_003",
		PaidAt:          1720000009,
	}

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	credited, duplicated, failed := 0, 0, 0
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

	assert.Equal(t, 1, credited, "exactly one credit must increase quota")
	assert.Equal(t, goroutines-1, duplicated, "rest are duplicates")
	assert.Equal(t, 0, failed, "no unexpected failures")

	// Durable state: credit is SUCCESS, quota increased exactly once.
	credit := reloadCredit(t, req.OrderNo)
	assert.Equal(t, WischoicerCreditStatusSuccess, credit.Status)
	assert.Equal(t, 500000, reloadUserQuota(t, 60003), "quota must increase by exactly one credit")
}

// ---------------------------------------------------------------------------
// 4. ConsumeReservedQuotaTx int32 hard cap — MySQL int column overflow rollback
// ---------------------------------------------------------------------------

// user.quota is a 32-bit int column on MySQL. When consumeQuotaForCreditTx runs
// `UPDATE users SET quota = quota + delta` and the result overflows signed
// int32, MySQL (strict mode) rejects the UPDATE — the whole transaction rolls
// back, so the CAS that flipped the credit RESERVED→SUCCESS is also undone.
// SQLite stores int64 regardless of column type, so this boundary is invisible
// there. This is the r5 overflow scenario lifted onto a real DB.
func TestConsumeReservedQuota_DatabaseInt32OverflowRollsBack(t *testing.T) {
	setupWischoicerIntegrationDB(t)
	// Bypass the soft limit by seeding directly: user.quota near MaxInt32.
	seedWischoicerUser(t, 60004, math.MaxInt32-5)
	seedWischoicerReservedCredit(t, &WischoicerRechargeCredit{
		OrderNo:         "ORDER_INT_OVF_004",
		NewAPIUserId:    60004,
		Quota:           100, // quota(MaxInt32-5) + 100 overflows int32
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
		Status:          WischoicerCreditStatusReserved,
	})

	_, err := CreditExternalRecharge(nil, CreditExternalRechargeRequest{
		OrderNo:         "ORDER_INT_OVF_004",
		NewApiUserId:    60004,
		Quota:           100,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
		TransactionId:   "TX_INT_OVF_004",
		PaidAt:          1720000010,
	})
	require.Error(t, err, "int32 overflow must cause the credit transaction to fail")

	// Durable state: credit rolled back to RESERVED (CAS undone), quota unchanged.
	credit := reloadCredit(t, "ORDER_INT_OVF_004")
	assert.Equal(t, WischoicerCreditStatusReserved, credit.Status,
		"CAS must roll back when the subsequent quota UPDATE fails")
	assert.Equal(t, math.MaxInt32-5, reloadUserQuota(t, 60004),
		"quota must be unchanged after overflow rollback")
}

// ---------------------------------------------------------------------------
// 5. RefundUserQuota int32 hard cap — CAS guard rejects reserved+current+delta > MaxInt32
// ---------------------------------------------------------------------------

// The refund fallback (refundUserQuotaDirectWithInt32Cap) guards the true hard
// cap: current + activeReserved + delta must not exceed MaxInt32. A refund that
// would overflow is rejected before the UPDATE. Two concurrent refunds that
// would each overflow must both be rejected — the user-row FOR UPDATE lock
// serializes them and neither writes. This is the r5 CAS subtraction guard.
func TestRefundUserQuota_DatabaseInt32OverflowWithReservationRejected(t *testing.T) {
	setupWischoicerIntegrationDB(t)
	setWischoicerCapacity(t, 1000000) // force CreditUserQuota to reject, exercising the fallback
	// Seed user near MaxInt32 (bypassing the soft limit) plus an active RESERVED
	// credit so reserved+current+delta overflows int32.
	seedWischoicerUser(t, 60005, math.MaxInt32-200)
	seedWischoicerReservedCredit(t, &WischoicerRechargeCredit{
		OrderNo:         "ORDER_INT_OVF_005",
		NewAPIUserId:    60005,
		Quota:           100,
		AmountCents:     1000,
		Currency:        "CNY",
		PaymentProvider: "wischoicer_wechat",
		Status:          WischoicerCreditStatusReserved,
	})

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		go func(idx int) {
			defer wg.Done()
			errs[idx] = RefundUserQuota(60005, 200)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		assert.Error(t, err, "refund %d must be rejected", i)
		assert.True(t, errors.Is(err, ErrWischoicerQuotaOverflow),
			"refund %d must be QUOTA_OVERFLOW, got %v", i, err)
	}

	// Durable state: quota unchanged; the RESERVED credit still counts against
	// the hard cap (not consumed by a partial refund).
	assert.Equal(t, math.MaxInt32-200, reloadUserQuota(t, 60005),
		"quota must be unchanged after both refunds rejected")
	credit := reloadCredit(t, "ORDER_INT_OVF_005")
	assert.Equal(t, WischoicerCreditStatusReserved, credit.Status)
}

// ---------------------------------------------------------------------------
// 6. DeleteUserById TOCTOU — user-row FOR UPDATE serializes reserve vs delete
// ---------------------------------------------------------------------------

// Concurrent reserve + delete race for the same user. The user-row FOR UPDATE
// lock serializes them: if a reservation commits first, delete sees it and is
// blocked; if delete commits first (soft-delete), reserve cannot find the user.
// The invariant — "a soft-deleted user never retains an active RESERVED
// reservation" — is what r3 flagged as a TOCTOU risk. SQLite's single-writer
// model hides the race; MySQL's record lock is the real serialization point.
func TestDeleteVsReserve_DatabaseNoLostReservation(t *testing.T) {
	setupWischoicerIntegrationDB(t)
	setWischoicerCapacity(t, 5000000)
	seedWischoicerUser(t, 60006, 0)

	const deleters, reservers = 2, 3
	var wg sync.WaitGroup
	wg.Add(deleters + reservers)

	for i := 0; i < deleters; i++ {
		go func() {
			defer wg.Done()
			_ = DeleteUserById(60006)
		}()
	}
	for i := 0; i < reservers; i++ {
		go func(idx int) {
			defer wg.Done()
			_, _ = ReserveExternalRecharge(nil, ReserveExternalRechargeRequest{
				OrderNo:         "ORDER_INT_RACE_" + string(rune('A'+idx)),
				NewApiUserId:    60006,
				Quota:           100000,
				AmountCents:     1000,
				Currency:        "CNY",
				PaymentProvider: "wischoicer_wechat",
			})
		}(i)
	}
	wg.Wait()

	// Invariant: a soft-deleted user (filtered by GORM's DeletedAt) must never
	// retain an active RESERVED reservation. The lock makes delete and reserve
	// mutually exclusive on the user row.
	var user User
	err := DB.Where("id = ?", 60006).First(&user).Error
	userGone := errors.Is(err, gorm.ErrRecordNotFound)
	// HasActiveWischoicerReservation queries without the soft-delete filter
	// (raw WHERE on the credits table), so it sees reservations even after the
	// user is soft-deleted — that is exactly the leak we must not have.
	hasReserved, _ := HasActiveWischoicerReservation(60006)
	if userGone && hasReserved {
		t.Fatalf("TOCTOU: user soft-deleted but an active RESERVED reservation remains — delete and reserve were not serialized")
	}
}

// ---------------------------------------------------------------------------
// 7. ReleaseExternalRecharge on a NotFound orderNo — idempotent nil
// ---------------------------------------------------------------------------

// release's contract (r6): a NotFound orderNo means "nothing to release" and
// must return nil so the billing release worker does not retry forever on a
// reserve-response-loss scenario. MySQL exercises the real First() miss path.
func TestReleaseExternalRecharge_DatabaseNotFoundIsIdempotent(t *testing.T) {
	setupWischoicerIntegrationDB(t)

	err := ReleaseExternalRecharge(nil, "ORDER_INT_NEVER_CREATED_007", "billing_release_fallback")
	assert.NoError(t, err, "release of a non-existent orderNo must be idempotent (nil)")

	// Durable state: no credit row materialized.
	var rowCount int64
	require.NoError(t, DB.Model(&WischoicerRechargeCredit{}).
		Where("order_no = ?", "ORDER_INT_NEVER_CREATED_007").Count(&rowCount).Error)
	assert.EqualValues(t, 0, rowCount)
}

// ---------------------------------------------------------------------------
// 8. CompleteEpayTopUpTx concurrent single credit — Epay trade_no row lock
// ---------------------------------------------------------------------------

// Two goroutines complete the same Epay topup (same trade_no). The top_ups row
// FOR UPDATE lock + the Pending→Success state check make exactly one credit
// (credited=true); the loser sees SUCCESS and returns credited=false without
// increasing quota. This is the r7 Epay atomic-transaction invariant. SQLite's
// single-writer model cannot prove the row lock; MySQL's record lock can.
func TestCompleteEpayTopUpTx_DatabaseConcurrentSingleCredit(t *testing.T) {
	setupWischoicerIntegrationDB(t)
	setWischoicerCapacity(t, 5000000)
	seedWischoicerUser(t, 60008, 0)

	const tradeNo = "EPAY_INT_008"
	const quotaToAdd = 500000
	seedTopUpRow(t, &TopUp{
		UserId:          60008,
		Amount:          1000,
		Money:           10.00,
		TradeNo:         tradeNo,
		PaymentMethod:   "alipay",
		PaymentProvider: PaymentProviderEpay,
		Status:          common.TopUpStatusPending,
	})

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	credited, notCredited, failed := 0, 0, 0
	var mu sync.Mutex

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			c, err := runCompleteEpayTopUpTx(t, tradeNo, quotaToAdd, "alipay", 1000)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				return
			}
			if c {
				credited++
			} else {
				notCredited++
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, credited, "exactly one goroutine should credit")
	assert.Equal(t, goroutines-1, notCredited, "rest should see already-success (no double credit)")
	assert.Equal(t, 0, failed, "no unexpected failures")

	// Durable state: topUp is SUCCESS, user quota increased exactly once.
	topUp := reloadTopUpByTradeNo(t, tradeNo)
	assert.Equal(t, common.TopUpStatusSuccess, topUp.Status)
	assert.Equal(t, quotaToAdd, reloadUserQuota(t, 60008), "quota must increase by exactly one credit")
}

func TestEpayMoneyMismatchAnomaly_DatabaseIdempotentObligation(t *testing.T) {
	setupWischoicerIntegrationDB(t)
	mmErr := &EpayMoneyMismatchError{
		TradeNo: "EPAY_DB_AUDIT_009", UserId: 60009, ExpectedCents: 1000, NotifyCents: 100,
	}
	const callbacks = 8
	errs := make(chan error, callbacks)
	var wg sync.WaitGroup
	for i := 0; i < callbacks; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- UpsertEpayMoneyMismatchAnomaly(mmErr, "203.0.113.20")
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	var anomalies []EpayPaymentAnomaly
	require.NoError(t, DB.Where("trade_no = ?", mmErr.TradeNo).Find(&anomalies).Error)
	require.Len(t, anomalies, 1)
	assert.EqualValues(t, callbacks, anomalies[0].OccurrenceCount)
	assert.Equal(t, EpayAnomalyStatusOpen, anomalies[0].Status)
}
