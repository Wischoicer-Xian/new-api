package model

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

var errSentinelOuter = errors.New("sentinel outer rollback")

// TestApplyBillingStageTx_OuterRollbackKeepsLedgerPending proves Tx version does
// not self-commit: when the outer caller returns a sentinel error AFTER
// ApplyBillingStageTx succeeds inside the transaction, the ledger CAS and
// callback writes all roll back to pending (§5.4 gate 1).
func TestApplyBillingStageTx_OuterRollbackKeepsLedgerPending(t *testing.T) {
	ledger := seedPendingLedger(t, "tx-outer-rollback")
	callbackCalled := false

	err := DB.Transaction(func(tx *gorm.DB) error {
		won, e := ApplyBillingStageTx(tx, ledger.ID, func(tx *gorm.DB, l *TaskBillingLedger) error {
			callbackCalled = true
			return nil
		})
		require.True(t, won)
		require.NoError(t, e)
		return errSentinelOuter // outer rolls back
	})
	assert.ErrorIs(t, err, errSentinelOuter)
	assert.True(t, callbackCalled, "callback was called inside the tx")

	// ledger reverted to pending
	var after TaskBillingLedger
	require.NoError(t, DB.First(&after, ledger.ID).Error)
	assert.Equal(t, BillingStatePending, after.State, "outer rollback reverts ledger to pending")
}

// TestApplyBillingStageTx_ApplyCallbackErrorRollsBack proves an apply callback
// error rolls back the applying claim and all callback writes (§5.4 gate 2).
func TestApplyBillingStageTx_ApplyCallbackErrorRollsBack(t *testing.T) {
	ledger := seedPendingLedger(t, "tx-callback-error")
	won, err := ApplyBillingStage(DB, ledger.ID, func(tx *gorm.DB, l *TaskBillingLedger) error {
		return errors.New("callback failure")
	})
	assert.False(t, won)
	assert.Error(t, err)

	var after TaskBillingLedger
	require.NoError(t, DB.First(&after, ledger.ID).Error)
	assert.Equal(t, BillingStatePending, after.State, "callback error reverts to pending")
}

// TestApplyBillingStageTx_AlreadyAppliedReplay proves a second invocation on an
// already-applied ledger returns won=false and does NOT invoke the callback
// (§5.4 gate 3).
func TestApplyBillingStageTx_AlreadyAppliedReplay(t *testing.T) {
	ledger := seedPendingLedger(t, "tx-replay")
	callbackCount := 0

	// First apply succeeds
	won1, err := ApplyBillingStage(DB, ledger.ID, func(tx *gorm.DB, l *TaskBillingLedger) error {
		callbackCount++
		return nil
	})
	require.NoError(t, err)
	require.True(t, won1)
	assert.Equal(t, 1, callbackCount)

	// Second apply: already applied, callback NOT called
	won2, err := ApplyBillingStage(DB, ledger.ID, func(tx *gorm.DB, l *TaskBillingLedger) error {
		callbackCount++
		return nil
	})
	require.NoError(t, err)
	assert.False(t, won2, "already-applied returns won=false")
	assert.Equal(t, 1, callbackCount, "callback not invoked on replay")
}

// TestApplyBillingStageTx_WrapperCompat proves the compatibility wrapper
// ApplyBillingStage still applies exactly once (§5.4 gate 4).
func TestApplyBillingStageTx_WrapperCompat(t *testing.T) {
	ledger := seedPendingLedger(t, "tx-wrapper")
	won, err := ApplyBillingStage(DB, ledger.ID, func(tx *gorm.DB, l *TaskBillingLedger) error {
		return nil
	})
	require.NoError(t, err)
	assert.True(t, won)

	var after TaskBillingLedger
	require.NoError(t, DB.First(&after, ledger.ID).Error)
	assert.Equal(t, BillingStateApplied, after.State)
}

// seedPendingLedger creates a pending-stage ledger row for testing.
func seedPendingLedger(t *testing.T, name string) *TaskBillingLedger {
	t.Helper()
	l := &TaskBillingLedger{
		TaskDBID:     90000 + int64(uniqueLedgerSeq()),
		Stage:        TaskBillingReserve,
		OperationKey: fmt.Sprintf("test_ledger_%s_%d", name, uniqueLedgerSeq()),
		State:        BillingStatePending,
		QuotaAmount:  5,
	}
	// Clean up this ledger after the test
	t.Cleanup(func() {
		DB.Where("id = ?", l.ID).Delete(&TaskBillingLedger{})
	})
	require.NoError(t, DB.Create(l).Error)
	return l
}

var ledgerSeq int64

func uniqueLedgerSeq() int64 {
	ledgerSeq++
	return ledgerSeq
}
