package model

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func truncateTaskBillingLedger(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		DB.Exec("DELETE FROM task_billing_ledgers")
	})
}

func billingStageIntent(taskDBID int64, stage TaskBillingStage, quotaAmount int) TaskBillingStageIntent {
	return TaskBillingStageIntent{TaskDBID: taskDBID, Stage: stage, QuotaAmount: quotaAmount}
}

func TestRecordBillingStage_CreatesPendingRow(t *testing.T) {
	truncateTaskBillingLedger(t)
	intent := billingStageIntent(101, TaskBillingReserve, 500)
	intent.Snapshot = json.RawMessage(`{"source":"wallet"}`)
	created, ledger, err := RecordBillingStage(DB, intent)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, BillingStatePending, ledger.State)
	assert.Equal(t, "billing:101:reserve", ledger.OperationKey)
	assert.Equal(t, 500, ledger.QuotaAmount)
}

func TestRecordBillingStage_ReplayReturnsExistingWithoutDuplicate(t *testing.T) {
	truncateTaskBillingLedger(t)
	_, first, err := RecordBillingStage(DB, billingStageIntent(102, TaskBillingReserve, 300))
	require.NoError(t, err)

	// A crash-recovery replay of the same stage must not create a second row.
	created, second, err := RecordBillingStage(DB, billingStageIntent(102, TaskBillingReserve, 300))
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, first.ID, second.ID)

	var count int64
	DB.Model(&TaskBillingLedger{}).Where("task_db_id = ?", 102).Count(&count)
	assert.EqualValues(t, 1, count)
}

func TestRecordBillingStage_StagesAreIndependent(t *testing.T) {
	truncateTaskBillingLedger(t)
	// reserve and settle are distinct idempotency units on the same task.
	_, _, err := RecordBillingStage(DB, billingStageIntent(103, TaskBillingReserve, 100))
	require.NoError(t, err)
	_, _, err = RecordBillingStage(DB, billingStageIntent(103, TaskBillingSettle, 90))
	require.NoError(t, err)
	_, _, err = RecordBillingStage(DB, billingStageIntent(103, TaskBillingRefund, 10))
	require.NoError(t, err)

	var count int64
	DB.Model(&TaskBillingLedger{}).Where("task_db_id = ?", 103).Count(&count)
	assert.EqualValues(t, 3, count)
}

func TestRecordBillingStage_ConcurrentSameStageConvergesToOne(t *testing.T) {
	useConcurrentSQLiteDB(t, "task_billing_ledger_concurrency", &TaskBillingLedger{})
	const n = 16
	var wg sync.WaitGroup
	var createdCount int32
	type outcome struct {
		created bool
		id      int64
		err     error
	}
	outcomes := make(chan outcome, n)
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			created, ledger, err := RecordBillingStage(DB, billingStageIntent(104, TaskBillingSettle, 200))
			if err == nil && created {
				atomic.AddInt32(&createdCount, 1)
			}
			result := outcome{created: created, err: err}
			if ledger != nil {
				result.id = ledger.ID
			}
			outcomes <- result
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)

	assert.Equal(t, int32(1), atomic.LoadInt32(&createdCount))
	var convergedID int64
	for result := range outcomes {
		require.NoError(t, result.err)
		require.NotZero(t, result.id)
		if convergedID == 0 {
			convergedID = result.id
		}
		assert.Equal(t, convergedID, result.id)
	}
	var count int64
	DB.Model(&TaskBillingLedger{}).Where("task_db_id = ? AND stage = ?", 104, TaskBillingSettle).Count(&count)
	assert.EqualValues(t, 1, count)
}

func TestApplyBillingStage_AppliesMutationAndStateAtomically(t *testing.T) {
	truncateTaskBillingLedger(t)
	_, ledger, err := RecordBillingStage(DB, billingStageIntent(105, TaskBillingReserve, 50))
	require.NoError(t, err)

	callbackCalls := 0
	won1, err := ApplyBillingStage(DB, ledger.ID, func(tx *gorm.DB, ledger *TaskBillingLedger) error {
		callbackCalls++
		return tx.Model(ledger).Update("attempt_count", 1).Error
	})
	require.NoError(t, err)
	won2, err := ApplyBillingStage(DB, ledger.ID, func(*gorm.DB, *TaskBillingLedger) error {
		callbackCalls++
		return nil
	})
	require.NoError(t, err)

	assert.True(t, won1)
	assert.False(t, won2)

	var final TaskBillingLedger
	require.NoError(t, DB.First(&final, ledger.ID).Error)
	assert.Equal(t, BillingStateApplied, final.State)
	assert.Equal(t, 1, final.AttemptCount)
	assert.Equal(t, 1, callbackCalls, "an applied replay must not invoke the quota mutation")
}

func TestApplyBillingStage_CallbackFailureRollsBackMutationAndState(t *testing.T) {
	truncateTaskBillingLedger(t)
	_, ledger, err := RecordBillingStage(DB, billingStageIntent(106, TaskBillingReserve, 50))
	require.NoError(t, err)
	wantErr := errors.New("quota update failed")

	won, err := ApplyBillingStage(DB, ledger.ID, func(tx *gorm.DB, ledger *TaskBillingLedger) error {
		require.NoError(t, tx.Model(ledger).Update("attempt_count", 1).Error)
		return wantErr
	})
	require.ErrorIs(t, err, wantErr)
	assert.False(t, won)

	var stored TaskBillingLedger
	require.NoError(t, DB.First(&stored, ledger.ID).Error)
	assert.Equal(t, BillingStatePending, stored.State)
	assert.Zero(t, stored.AttemptCount)
}

func TestApplyBillingStage_ConcurrentOnlyOneCallbackRuns(t *testing.T) {
	useConcurrentSQLiteDB(t, "task_billing_apply_concurrency", &TaskBillingLedger{})
	_, ledger, err := RecordBillingStage(DB, billingStageIntent(108, TaskBillingSettle, 50))
	require.NoError(t, err)

	const workers = 8
	type outcome struct {
		won bool
		err error
	}
	outcomes := make(chan outcome, workers)
	start := make(chan struct{})
	var callbackCalls int32
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			<-start
			won, err := ApplyBillingStage(DB, ledger.ID, func(tx *gorm.DB, ledger *TaskBillingLedger) error {
				atomic.AddInt32(&callbackCalls, 1)
				return tx.Model(ledger).UpdateColumn("attempt_count", gorm.Expr("attempt_count + 1")).Error
			})
			outcomes <- outcome{won: won, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)

	winners := 0
	for result := range outcomes {
		require.NoError(t, result.err)
		if result.won {
			winners++
		}
	}
	assert.Equal(t, 1, winners)
	assert.Equal(t, int32(1), atomic.LoadInt32(&callbackCalls))
	var stored TaskBillingLedger
	require.NoError(t, DB.First(&stored, ledger.ID).Error)
	assert.Equal(t, BillingStateApplied, stored.State)
	assert.Equal(t, 1, stored.AttemptCount)
}

func TestRecordBillingStage_RejectsInvalidStageAndNegativeQuota(t *testing.T) {
	truncateTaskBillingLedger(t)

	_, _, err := RecordBillingStage(DB, billingStageIntent(107, TaskBillingStage("credit"), 1))
	require.ErrorIs(t, err, ErrInvalidBillingStage)
	_, _, err = RecordBillingStage(DB, billingStageIntent(107, TaskBillingReserve, -1))
	require.ErrorIs(t, err, ErrNegativeQuotaAmount)
	_, _, err = RecordBillingStage(DB, billingStageIntent(107, TaskBillingReserve, common.MaxQuota+1))
	require.Error(t, err)
	invalidSnapshot := billingStageIntent(107, TaskBillingReserve, 1)
	invalidSnapshot.Snapshot = json.RawMessage(`{"broken"`)
	_, _, err = RecordBillingStage(DB, invalidSnapshot)
	require.Error(t, err)

	var count int64
	require.NoError(t, DB.Model(&TaskBillingLedger{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestBillingOperationKey_StableShape(t *testing.T) {
	assert.Equal(t, "billing:7:settle", BillingOperationKey(7, TaskBillingSettle))
	// Stability across calls is what lets a replayer find the prior record.
	assert.Equal(t, BillingOperationKey(7, TaskBillingSettle), BillingOperationKey(7, TaskBillingSettle))
}
