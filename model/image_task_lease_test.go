package model

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func insertClaimableExecution(t *testing.T, state ImageTaskExecutionState, nextRunAt, leaseUntil int64, owner string) *ImageTaskExecution {
	t.Helper()
	seq := atomic.AddInt64(&execSeq, 1)
	exec := &ImageTaskExecution{
		PublicTaskID:    "imgtask_lease_" + string(rune('a'+int(seq%26))) + string(rune('a'+int(seq/26))),
		TaskDBID:        seq,
		OwnerUserID:     1,
		Operation:       ImageTaskOperationGeneration,
		IdempotencyKey:  "lease-key-" + string(rune('a'+int(seq%26))) + string(rune('a'+int(seq/26))),
		RequestHash:     "h",
		State:           state,
		NextRunAt:       nextRunAt,
		LeaseOwner:      owner,
		LeaseUntil:      leaseUntil,
		LeaseGeneration: 0,
	}
	require.NoError(t, DB.Create(exec).Error)
	return exec
}

func TestTryClaim_DueAndUnleasedSucceeds(t *testing.T) {
	truncateImageTaskExecutions(t)
	exec := insertClaimableExecution(t, ImageTaskStateQueued, 100, 0, "")

	won, claimed, err := TryClaimImageTaskExecution(ImageTaskLeaseClaim{ExecutionID: exec.ID, Owner: "worker-A", Now: 100, LeaseUntil: 160})
	require.NoError(t, err)
	require.True(t, won)
	assert.Equal(t, "worker-A", claimed.LeaseOwner)
	assert.Equal(t, int64(160), claimed.LeaseUntil)
	assert.Equal(t, 1, claimed.LeaseGeneration, "generation must increment to fence stale workers")
}

func TestTryClaim_NotDueFails(t *testing.T) {
	truncateImageTaskExecutions(t)
	// next_run_at is in the future relative to now.
	exec := insertClaimableExecution(t, ImageTaskStateQueued, 200, 0, "")

	won, _, err := TryClaimImageTaskExecution(ImageTaskLeaseClaim{ExecutionID: exec.ID, Owner: "worker-A", Now: 100, LeaseUntil: 160})
	require.NoError(t, err)
	assert.False(t, won)
}

func TestTryClaim_UnexpiredLeaseBlocksOther(t *testing.T) {
	truncateImageTaskExecutions(t)
	// worker-A holds a lease until 300; at now=100 it has not expired.
	exec := insertClaimableExecution(t, ImageTaskStatePolling, 50, 300, "worker-A")

	won, _, err := TryClaimImageTaskExecution(ImageTaskLeaseClaim{ExecutionID: exec.ID, Owner: "worker-B", Now: 100, LeaseUntil: 160})
	require.NoError(t, err)
	assert.False(t, won, "a second worker must not steal an unexpired lease")
}

func TestTryClaim_LeaseAtNowIsStillOwned(t *testing.T) {
	truncateImageTaskExecutions(t)
	exec := insertClaimableExecution(t, ImageTaskStatePolling, 50, 100, "worker-A")

	won, _, err := TryClaimImageTaskExecution(ImageTaskLeaseClaim{ExecutionID: exec.ID, Owner: "worker-B", Now: 100, LeaseUntil: 160})
	require.NoError(t, err)
	assert.False(t, won, "claim uses lease_until < now, so equality must remain leased")
}

func TestTryClaim_ExpiredLeaseCanBeTakenOver(t *testing.T) {
	truncateImageTaskExecutions(t)
	// worker-A's lease expired at 50; at now=100 worker-B may take over.
	exec := insertClaimableExecution(t, ImageTaskStatePolling, 40, 50, "worker-A")
	exec.LeaseGeneration = 1
	require.NoError(t, DB.Model(&ImageTaskExecution{}).Where("id = ?", exec.ID).Update("lease_generation", 1).Error)

	won, claimed, err := TryClaimImageTaskExecution(ImageTaskLeaseClaim{ExecutionID: exec.ID, Owner: "worker-B", Now: 100, LeaseUntil: 160})
	require.NoError(t, err)
	require.True(t, won)
	assert.Equal(t, "worker-B", claimed.LeaseOwner)
	assert.Equal(t, 2, claimed.LeaseGeneration, "takeover must bump generation again to fence the stalled worker")
}

func TestTryClaim_TerminalStateNotClaimable(t *testing.T) {
	truncateImageTaskExecutions(t)
	exec := insertClaimableExecution(t, ImageTaskStateCompleted, 50, 0, "")

	won, _, err := TryClaimImageTaskExecution(ImageTaskLeaseClaim{ExecutionID: exec.ID, Owner: "worker-A", Now: 100, LeaseUntil: 160})
	require.NoError(t, err)
	assert.False(t, won, "a terminal task must never be claimed")
}

func TestTryClaim_ConcurrentOnlyOneWins(t *testing.T) {
	useConcurrentSQLiteDB(t, "image_task_lease_concurrency", &ImageTaskExecution{})
	exec := insertClaimableExecution(t, ImageTaskStateQueued, 50, 0, "")

	type outcome struct {
		won bool
		err error
	}
	outcomes := make(chan outcome, 8)
	var wg sync.WaitGroup
	wg.Add(8)
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(n int) {
			defer wg.Done()
			<-start
			won, _, err := TryClaimImageTaskExecution(ImageTaskLeaseClaim{ExecutionID: exec.ID, Owner: fmt.Sprintf("worker-%d", n), Now: 100, LeaseUntil: 160})
			outcomes <- outcome{won: won, err: err}
		}(i)
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
	assert.Equal(t, 1, winners, "only one worker may claim the lease")
}

func TestRenewLease_OnlyAtExpectedGeneration(t *testing.T) {
	truncateImageTaskExecutions(t)
	exec := insertClaimableExecution(t, ImageTaskStatePolling, 50, 100, "worker-A")
	exec.LeaseGeneration = 1
	require.NoError(t, DB.Model(&ImageTaskExecution{}).Where("id = ?", exec.ID).Update("lease_generation", 1).Error)

	// Holder renews at the right generation: succeeds.
	won, err := RenewImageTaskExecutionLease(ImageTaskLeaseRenewal{ExecutionID: exec.ID, Owner: "worker-A", ExpectedGeneration: 1, Now: 90, LeaseUntil: 200})
	require.NoError(t, err)
	assert.True(t, won)

	// Stalled worker (generation 0) tries to renew: must be rejected.
	won, err = RenewImageTaskExecutionLease(ImageTaskLeaseRenewal{ExecutionID: exec.ID, Owner: "worker-A", ExpectedGeneration: 0, Now: 90, LeaseUntil: 300})
	require.NoError(t, err)
	assert.False(t, won, "a stale generation must not renew the lease")
}

func TestRenewLease_ExpiredLeaseCannotBeResurrected(t *testing.T) {
	truncateImageTaskExecutions(t)
	exec := insertClaimableExecution(t, ImageTaskStatePolling, 50, 100, "worker-A")
	exec.LeaseGeneration = 1
	require.NoError(t, DB.Model(&ImageTaskExecution{}).Where("id = ?", exec.ID).Update("lease_generation", 1).Error)

	won, err := RenewImageTaskExecutionLease(ImageTaskLeaseRenewal{ExecutionID: exec.ID, Owner: "worker-A", ExpectedGeneration: 1, Now: 101, LeaseUntil: 200})
	require.NoError(t, err)
	assert.False(t, won)
}

func TestTryClaim_RejectsInvalidLeaseBounds(t *testing.T) {
	truncateImageTaskExecutions(t)
	exec := insertClaimableExecution(t, ImageTaskStateQueued, 50, 0, "")

	_, _, err := TryClaimImageTaskExecution(ImageTaskLeaseClaim{ExecutionID: exec.ID, Now: 100, LeaseUntil: 160})
	require.Error(t, err)
	_, _, err = TryClaimImageTaskExecution(ImageTaskLeaseClaim{ExecutionID: exec.ID, Owner: "worker-A", Now: 100, LeaseUntil: 100})
	require.Error(t, err)
}

func TestIsClaimableImageTaskState(t *testing.T) {
	tests := []struct {
		state     ImageTaskExecutionState
		terminal  bool
		claimable bool
	}{
		{state: ImageTaskStateQueued, claimable: true},
		{state: ImageTaskStateSubmitting, claimable: true},
		{state: ImageTaskStateSubmissionUnknown, claimable: true},
		{state: ImageTaskStatePolling, claimable: true},
		{state: ImageTaskStateCancelRequested, claimable: true},
		{state: ImageTaskStateCompleted, terminal: true},
		{state: ImageTaskStateFailed, terminal: true},
		{state: ImageTaskStateCancelled, terminal: true},
		{state: ImageTaskStateManualReview},
	}
	for _, test := range tests {
		t.Run(string(test.state), func(t *testing.T) {
			assert.Equal(t, test.terminal, IsTerminalImageTaskState(test.state))
			assert.Equal(t, test.claimable, IsClaimableImageTaskState(test.state))
			assert.False(t, test.terminal && test.claimable, "a state must never be terminal and claimable")
		})
	}
}
