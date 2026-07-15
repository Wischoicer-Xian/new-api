package model

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// execSeq allocates unique PublicTaskID / TaskDBID values per execution so
// the idempotency-key fixture does not collide on those independent identity
// columns. Only owner/operation/key are intentionally shared to exercise the
// idempotency namespace.
var execSeq int64

func newImageTaskExecution(owner int, op, key, hash string) *ImageTaskExecution {
	seq := atomic.AddInt64(&execSeq, 1)
	return &ImageTaskExecution{
		PublicTaskID:   fmt.Sprintf("imgtask_%d", seq),
		TaskDBID:       seq,
		OwnerUserID:    owner,
		Operation:      op,
		IdempotencyKey: key,
		RequestHash:    hash,
		State:          ImageTaskStateQueued,
	}
}

func terminalTransition(exec *ImageTaskExecution, claimed *ImageTaskExecution, owner string, now int64, from ImageTaskExecutionState) ImageTaskTerminalTransition {
	return ImageTaskTerminalTransition{
		ID:                 exec.ID,
		LeaseOwner:         owner,
		ExpectedGeneration: claimed.LeaseGeneration,
		Now:                now,
		From:               from,
		To:                 ImageTaskStateCompleted,
	}
}

func noTerminalSideEffects(*gorm.DB) error { return nil }

func truncateImageTaskExecutions(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		DB.Exec("DELETE FROM image_task_executions")
	})
}

func TestCreateOrGetImageTaskExecution_CreatesNewOnFirstInsert(t *testing.T) {
	truncateImageTaskExecutions(t)
	exec := newImageTaskExecution(1, ImageTaskOperationGeneration, "key-1", "hash-1")

	created, stored, err := CreateOrGetImageTaskExecution(DB, exec)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, ImageTaskStateQueued, stored.State)
	assert.NotZero(t, stored.ID)
}

func TestCreateOrGetImageTaskExecution_ReplaysSameHash(t *testing.T) {
	truncateImageTaskExecutions(t)
	exec := newImageTaskExecution(1, ImageTaskOperationGeneration, "key-2", "hash-2")
	_, original, err := CreateOrGetImageTaskExecution(DB, exec)
	require.NoError(t, err)

	// A second request with the same key and same canonical hash must replay
	// the original task rather than create a duplicate.
	replay := newImageTaskExecution(1, ImageTaskOperationGeneration, "key-2", "hash-2")
	created, stored, err := CreateOrGetImageTaskExecution(DB, replay)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, original.ID, stored.ID)
}

func TestCreateOrGetImageTaskExecution_ConflictOnDifferentHash(t *testing.T) {
	truncateImageTaskExecutions(t)
	_, _, err := CreateOrGetImageTaskExecution(DB, newImageTaskExecution(1, ImageTaskOperationGeneration, "key-3", "hash-a"))
	require.NoError(t, err)

	// Same idempotency key but a different request body is a conflict: the
	// caller is trying to bind the same key to a different request.
	_, stored, err := CreateOrGetImageTaskExecution(DB, newImageTaskExecution(1, ImageTaskOperationGeneration, "key-3", "hash-b"))
	require.ErrorIs(t, err, ErrIdempotencyConflict)
	require.NotNil(t, stored)
	assert.Equal(t, "hash-a", stored.RequestHash, "stored row must be the original, not the conflicting one")
}

func TestCreateOrGetImageTaskExecution_NamespaceIsolation(t *testing.T) {
	truncateImageTaskExecutions(t)
	// Same key under a different owner is a separate namespace and must
	// create independently.
	_, _, err := CreateOrGetImageTaskExecution(DB, newImageTaskExecution(1, ImageTaskOperationGeneration, "shared-key", "h"))
	require.NoError(t, err)
	_, _, err = CreateOrGetImageTaskExecution(DB, newImageTaskExecution(2, ImageTaskOperationGeneration, "shared-key", "h"))
	require.NoError(t, err)

	var count int64
	DB.Model(&ImageTaskExecution{}).Where("idempotency_key = ?", "shared-key").Count(&count)
	assert.EqualValues(t, 2, count)
}

func TestCreateOrGetImageTaskExecution_ConcurrentSameKeyConvergesToOne(t *testing.T) {
	useConcurrentSQLiteDB(t, "image_task_execution_concurrency", &ImageTaskExecution{}, &ChannelRevision{})
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
			exec := newImageTaskExecution(1, ImageTaskOperationGeneration, "race-key", "race-hash")
			created, stored, err := CreateOrGetImageTaskExecution(DB, exec)
			if err != nil {
				outcomes <- outcome{err: err}
				return
			}
			if created {
				atomic.AddInt32(&createdCount, 1)
			}
			outcomes <- outcome{created: created, id: stored.ID}
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)

	assert.Equal(t, int32(1), atomic.LoadInt32(&createdCount), "exactly one goroutine must create the row")
	var convergedID int64
	for result := range outcomes {
		require.NoError(t, result.err)
		require.NotZero(t, result.id)
		if convergedID == 0 {
			convergedID = result.id
		}
		assert.Equal(t, convergedID, result.id, "every caller must receive the same stored row")
	}

	var count int64
	DB.Model(&ImageTaskExecution{}).Where("idempotency_key = ?", "race-key").Count(&count)
	assert.EqualValues(t, 1, count, "only one row must exist for the key")
}

func TestMarkImageTaskTerminalCAS_OnlyOneWinner(t *testing.T) {
	useConcurrentSQLiteDB(t, "image_task_finalize_concurrency", &ImageTaskExecution{}, &ChannelRevision{})
	_, exec, err := CreateOrGetImageTaskExecution(DB, newImageTaskExecution(1, ImageTaskOperationGeneration, "cas-key", "h"))
	require.NoError(t, err)
	won, claimed, err := TryClaimImageTaskExecution(ImageTaskLeaseClaim{ExecutionID: exec.ID, Owner: "worker-A", Now: 100, LeaseUntil: 200})
	require.NoError(t, err)
	require.True(t, won)

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
			won, err := FinalizeImageTaskExecutionCAS(DB, terminalTransition(exec, claimed, "worker-A", 150, ImageTaskStateQueued), func(tx *gorm.DB) error {
				atomic.AddInt32(&callbackCalls, 1)
				return tx.Model(&ImageTaskExecution{}).Where("id = ?", exec.ID).Update("finished_at", 150).Error
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
	assert.Equal(t, int32(1), atomic.LoadInt32(&callbackCalls), "terminal side effects must run exactly once")
}

func TestMarkImageTaskTerminalCAS_RejectsWrongFromState(t *testing.T) {
	truncateImageTaskExecutions(t)
	_, exec, err := CreateOrGetImageTaskExecution(DB, newImageTaskExecution(1, ImageTaskOperationGeneration, "cas-key-2", "h"))
	require.NoError(t, err)
	won, claimed, err := TryClaimImageTaskExecution(ImageTaskLeaseClaim{ExecutionID: exec.ID, Owner: "worker-A", Now: 100, LeaseUntil: 200})
	require.NoError(t, err)
	require.True(t, won)

	// The row is queued; attempting to transition from polling must not match.
	won, err = FinalizeImageTaskExecutionCAS(DB, terminalTransition(exec, claimed, "worker-A", 150, ImageTaskStatePolling), noTerminalSideEffects)
	require.NoError(t, err)
	assert.False(t, won)
}

func TestMarkImageTaskTerminalCAS_RejectsStaleLeaseGeneration(t *testing.T) {
	truncateImageTaskExecutions(t)
	_, exec, err := CreateOrGetImageTaskExecution(DB, newImageTaskExecution(1, ImageTaskOperationGeneration, "cas-stale", "h"))
	require.NoError(t, err)

	won, first, err := TryClaimImageTaskExecution(ImageTaskLeaseClaim{ExecutionID: exec.ID, Owner: "worker-A", Now: 100, LeaseUntil: 120})
	require.NoError(t, err)
	require.True(t, won)
	won, second, err := TryClaimImageTaskExecution(ImageTaskLeaseClaim{ExecutionID: exec.ID, Owner: "worker-B", Now: 121, LeaseUntil: 200})
	require.NoError(t, err)
	require.True(t, won)

	won, err = FinalizeImageTaskExecutionCAS(DB, terminalTransition(exec, first, "worker-A", 121, ImageTaskStateQueued), noTerminalSideEffects)
	require.NoError(t, err)
	assert.False(t, won)
	won, err = FinalizeImageTaskExecutionCAS(DB, terminalTransition(exec, second, "worker-B", 150, ImageTaskStateQueued), noTerminalSideEffects)
	require.NoError(t, err)
	assert.True(t, won)
}

func TestFinalizeImageTaskExecutionCAS_CallbackFailureRollsBackTerminalState(t *testing.T) {
	truncateImageTaskExecutions(t)
	_, exec, err := CreateOrGetImageTaskExecution(DB, newImageTaskExecution(1, ImageTaskOperationGeneration, "cas-rollback", "h"))
	require.NoError(t, err)
	won, claimed, err := TryClaimImageTaskExecution(ImageTaskLeaseClaim{ExecutionID: exec.ID, Owner: "worker-A", Now: 100, LeaseUntil: 200})
	require.NoError(t, err)
	require.True(t, won)
	wantErr := errors.New("legacy projection failed")

	won, err = FinalizeImageTaskExecutionCAS(DB, terminalTransition(exec, claimed, "worker-A", 150, ImageTaskStateQueued), func(tx *gorm.DB) error {
		require.NoError(t, tx.Model(&ImageTaskExecution{}).Where("id = ?", exec.ID).Update("finished_at", 150).Error)
		return wantErr
	})
	require.ErrorIs(t, err, wantErr)
	assert.False(t, won)

	var stored ImageTaskExecution
	require.NoError(t, DB.First(&stored, exec.ID).Error)
	assert.Equal(t, ImageTaskStateQueued, stored.State)
	assert.Zero(t, stored.FinishedAt)
}
