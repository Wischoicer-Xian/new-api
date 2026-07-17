package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestAdvanceCAS_RejectsStaleGeneration pins the fencing invariant of the
// processor's non-terminal advance: a worker presenting a stale lease_generation
// (because its lease expired and another worker took over) must lose the CAS and
// mutate nothing. This is the correctness core that makes the processor safe
// under lease expiry — without it a slow worker could clobber a live one's state.
func TestAdvanceCAS_RejectsStaleGeneration(t *testing.T) {
	truncateImageTaskExecutions(t)
	exec := insertClaimableExecution(t, ImageTaskStateQueued, 100, 0, "")

	// Worker A claims the execution.
	won, claimed, err := TryClaimImageTaskExecution(ImageTaskLeaseClaim{ExecutionID: exec.ID, Owner: "A", Now: 100, LeaseUntil: 200})
	require.NoError(t, err)
	require.True(t, won)
	require.Equal(t, 1, claimed.LeaseGeneration)

	// Worker A advances with the correct generation → wins.
	adv := ImageTaskAdvance{ID: exec.ID, LeaseOwner: "A", ExpectedGeneration: 1, Now: 150, From: ImageTaskStateQueued, To: ImageTaskStatePolling}
	won, err = AdvanceImageTaskExecutionCAS(adv, func(tx *gorm.DB) error {
		return tx.Model(&ImageTaskExecution{}).Where("id = ?", exec.ID).Update("client_submission_id", "task_adv_1").Error
	})
	require.NoError(t, err)
	assert.True(t, won)
	got := reloadAdvExecution(t, exec.ID)
	assert.Equal(t, ImageTaskStatePolling, got.State)
	assert.Equal(t, "task_adv_1", got.ClientSubmissionID)

	// A stale worker (generation 0, the pre-claim value) tries to advance from
	// polling back to queued → must lose, leaving the live state untouched.
	stale := ImageTaskAdvance{ID: exec.ID, LeaseOwner: "A", ExpectedGeneration: 0, Now: 160, From: ImageTaskStatePolling, To: ImageTaskStateQueued}
	won, err = AdvanceImageTaskExecutionCAS(stale, func(tx *gorm.DB) error {
		return tx.Model(&ImageTaskExecution{}).Where("id = ?", exec.ID).Update("client_submission_id", "STALE").Error
	})
	require.NoError(t, err)
	assert.False(t, won, "stale generation must lose the CAS")
	got = reloadAdvExecution(t, exec.ID)
	assert.Equal(t, ImageTaskStatePolling, got.State, "live state must be unchanged")
	assert.Equal(t, "task_adv_1", got.ClientSubmissionID, "stale worker must not overwrite side writes")
}

// TestAdvanceCAS_RejectsTerminalTargets verifies the guardrail: terminal
// transitions must go through FinalizeImageTask (billing in the CAS callback),
// not AdvanceImageTaskExecutionCAS.
func TestAdvanceCAS_RejectsTerminalTargets(t *testing.T) {
	truncateImageTaskExecutions(t)
	exec := insertClaimableExecution(t, ImageTaskStatePolling, 100, 0, "")
	won, claimed, err := TryClaimImageTaskExecution(ImageTaskLeaseClaim{ExecutionID: exec.ID, Owner: "A", Now: 100, LeaseUntil: 200})
	require.NoError(t, err)
	require.True(t, won)

	_, err = AdvanceImageTaskExecutionCAS(ImageTaskAdvance{ID: exec.ID, LeaseOwner: "A", ExpectedGeneration: claimed.LeaseGeneration, Now: 150, From: ImageTaskStatePolling, To: ImageTaskStateCompleted}, nil)
	require.Error(t, err)
}

// TestMarkManualReviewCAS transitions a submission_unknown execution into
// manual_review under the lease fence.
func TestMarkManualReviewCAS(t *testing.T) {
	truncateImageTaskExecutions(t)
	exec := insertClaimableExecution(t, ImageTaskStateSubmissionUnknown, 100, 0, "")
	won, claimed, err := TryClaimImageTaskExecution(ImageTaskLeaseClaim{ExecutionID: exec.ID, Owner: "A", Now: 100, LeaseUntil: 200})
	require.NoError(t, err)
	require.True(t, won)

	won, err = MarkImageTaskManualReviewCAS(ImageTaskAdvance{ID: exec.ID, LeaseOwner: "A", ExpectedGeneration: claimed.LeaseGeneration, Now: 150, From: ImageTaskStateSubmissionUnknown}, "submission unknown past SLA")
	require.NoError(t, err)
	assert.True(t, won)
	got := reloadAdvExecution(t, exec.ID)
	assert.Equal(t, ImageTaskStateManualReview, got.State)
	assert.Equal(t, "submission unknown past SLA", got.ManualReviewReason)
	assert.Equal(t, int64(0), got.NextRunAt, "manual_review clears next_run_at so no auto-processing resumes")
}

func reloadAdvExecution(t *testing.T, id int64) ImageTaskExecution {
	t.Helper()
	var exec ImageTaskExecution
	require.NoError(t, DB.First(&exec, id).Error)
	return exec
}

// TestHasDueImageTaskExecutions verifies the system-task Enabled() gate: it is
// true only when a claimable execution is due. HasDueImageTaskExecutions uses
// common.GetTimestamp internally, so the fixture seeds values relative to now.
func TestHasDueImageTaskExecutions(t *testing.T) {
	truncateImageTaskExecutions(t)
	assert.False(t, HasDueImageTaskExecutions(), "no executions → not due")

	now := common.GetTimestamp()
	// next_run_at in the future → not due.
	insertClaimableExecution(t, ImageTaskStateQueued, now+3600, 0, "")
	assert.False(t, HasDueImageTaskExecutions())

	// next_run_at in the past → due.
	insertClaimableExecution(t, ImageTaskStatePolling, now-100, 0, "")
	assert.True(t, HasDueImageTaskExecutions())
}
