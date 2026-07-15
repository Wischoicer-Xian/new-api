//go:build integration

package model

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestImageTaskExecution_DatabaseConcurrentIdempotencyConverges(t *testing.T) {
	setupWischoicerIntegrationDB(t)
	const workers = 16
	type outcome struct {
		created bool
		id      int64
		err     error
	}
	outcomes := make(chan outcome, workers)
	start := make(chan struct{})
	var sequence int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			<-start
			id := atomic.AddInt64(&sequence, 1)
			exec := &ImageTaskExecution{
				PublicTaskID:   fmt.Sprintf("imgtask_integration_%d", id),
				TaskDBID:       id,
				OwnerUserID:    42,
				Operation:      ImageTaskOperationGeneration,
				IdempotencyKey: "integration-key",
				RequestHash:    "same-hash",
				State:          ImageTaskStateQueued,
			}
			created, stored, err := CreateOrGetImageTaskExecution(DB, exec)
			result := outcome{created: created, err: err}
			if stored != nil {
				result.id = stored.ID
			}
			outcomes <- result
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)

	createdCount := 0
	var convergedID int64
	for result := range outcomes {
		require.NoError(t, result.err)
		require.NotZero(t, result.id)
		if result.created {
			createdCount++
		}
		if convergedID == 0 {
			convergedID = result.id
		}
		assert.Equal(t, convergedID, result.id)
	}
	assert.Equal(t, 1, createdCount)
	var count int64
	require.NoError(t, DB.Model(&ImageTaskExecution{}).Where("owner_user_id = ? AND operation = ? AND idempotency_key = ?", 42, ImageTaskOperationGeneration, "integration-key").Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

func TestTaskBillingLedger_DatabaseConcurrentRecordAndApplyConverge(t *testing.T) {
	setupWischoicerIntegrationDB(t)
	const workers = 16
	type recordOutcome struct {
		created bool
		id      int64
		err     error
	}
	records := make(chan recordOutcome, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			<-start
			created, ledger, err := RecordBillingStage(DB, TaskBillingStageIntent{TaskDBID: 501, Stage: TaskBillingReserve, QuotaAmount: 100})
			result := recordOutcome{created: created, err: err}
			if ledger != nil {
				result.id = ledger.ID
			}
			records <- result
		}()
	}
	close(start)
	wg.Wait()
	close(records)

	createdCount := 0
	var ledgerID int64
	for result := range records {
		require.NoError(t, result.err)
		if result.created {
			createdCount++
		}
		if ledgerID == 0 {
			ledgerID = result.id
		}
		assert.Equal(t, ledgerID, result.id)
	}
	assert.Equal(t, 1, createdCount)

	type applyOutcome struct {
		won bool
		err error
	}
	applies := make(chan applyOutcome, workers)
	start = make(chan struct{})
	var callbackCalls int32
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			<-start
			won, err := ApplyBillingStage(DB, ledgerID, func(tx *gorm.DB, ledger *TaskBillingLedger) error {
				atomic.AddInt32(&callbackCalls, 1)
				return tx.Model(ledger).UpdateColumn("attempt_count", gorm.Expr("attempt_count + 1")).Error
			})
			applies <- applyOutcome{won: won, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(applies)

	winners := 0
	for result := range applies {
		require.NoError(t, result.err)
		if result.won {
			winners++
		}
	}
	assert.Equal(t, 1, winners)
	assert.Equal(t, int32(1), atomic.LoadInt32(&callbackCalls))
	var stored TaskBillingLedger
	require.NoError(t, DB.First(&stored, ledgerID).Error)
	assert.Equal(t, BillingStateApplied, stored.State)
	assert.Equal(t, 1, stored.AttemptCount)
}

func TestImageTaskLease_DatabaseGenerationFencesStaleFinalize(t *testing.T) {
	setupWischoicerIntegrationDB(t)
	exec := &ImageTaskExecution{
		PublicTaskID: "imgtask_lease_integration", TaskDBID: 601, OwnerUserID: 42,
		Operation: ImageTaskOperationGeneration, IdempotencyKey: "lease-integration", RequestHash: "hash",
		State: ImageTaskStatePolling, NextRunAt: 100,
	}
	require.NoError(t, DB.Create(exec).Error)

	won, first, err := TryClaimImageTaskExecution(ImageTaskLeaseClaim{ExecutionID: exec.ID, Owner: "worker-a", Now: 100, LeaseUntil: 120})
	require.NoError(t, err)
	require.True(t, won)
	won, second, err := TryClaimImageTaskExecution(ImageTaskLeaseClaim{ExecutionID: exec.ID, Owner: "worker-b", Now: 121, LeaseUntil: 200})
	require.NoError(t, err)
	require.True(t, won)

	won, err = FinalizeImageTaskExecutionCAS(DB, terminalTransition(exec, first, "worker-a", 121, ImageTaskStatePolling), noTerminalSideEffects)
	require.NoError(t, err)
	assert.False(t, won)
	won, err = FinalizeImageTaskExecutionCAS(DB, terminalTransition(exec, second, "worker-b", 150, ImageTaskStatePolling), noTerminalSideEffects)
	require.NoError(t, err)
	assert.True(t, won)
}

func TestChannelRevision_DatabaseConcurrentAllocationIsGapFree(t *testing.T) {
	setupWischoicerIntegrationDB(t)
	require.NoError(t, DB.Create(&Channel{Id: 701, Key: "integration-key"}).Error)
	const workers = 8
	type outcome struct {
		number int
		err    error
	}
	outcomes := make(chan outcome, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			<-start
			revision, err := CreateChannelRevision(ChannelRevisionCreate{ChannelID: 701, Endpoint: "https://example.com", CredentialRef: "credential", AdapterVersion: "v1"})
			result := outcome{err: err}
			if revision != nil {
				result.number = revision.RevisionNumber
			}
			outcomes <- result
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)

	seen := make(map[int]bool, workers)
	for result := range outcomes {
		require.NoError(t, result.err)
		seen[result.number] = true
	}
	for number := 1; number <= workers; number++ {
		assert.True(t, seen[number], "revision number %d must be allocated", number)
	}
}
