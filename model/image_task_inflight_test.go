package model

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCountInFlightImageTasksByOwner proves only non-terminal image task
// executions count toward a user's in-flight cap (§6.1): completed/failed/
// cancelled free capacity; every other state (queued, submitting,
// submission_unknown, polling, cancel_requested, manual_review) holds it.
func TestCountInFlightImageTasksByOwner(t *testing.T) {
	seedInFlightRows(t, 1)
	seedInFlightRows(t, 2) // other user, must not affect owner 1

	count, err := CountInFlightImageTasksByOwner(1)
	require.NoError(t, err)
	assert.Equal(t, int64(6), count, "six non-terminal states for owner 1")

	other, err := CountInFlightImageTasksByOwner(2)
	require.NoError(t, err)
	assert.Equal(t, int64(6), other, "owner 2 isolated, same six non-terminal states")

	none, err := CountInFlightImageTasksByOwner(999)
	require.NoError(t, err)
	assert.Zero(t, none, "unknown owner has no in-flight tasks")
}

// seedInFlightRows inserts one row in every state for the given owner.
func seedInFlightRows(t *testing.T, owner int) {
	t.Helper()
	states := []ImageTaskExecutionState{
		ImageTaskStateQueued,            // non-terminal
		ImageTaskStateSubmitting,        // non-terminal
		ImageTaskStateSubmissionUnknown, // non-terminal
		ImageTaskStatePolling,           // non-terminal
		ImageTaskStateCancelRequested,   // non-terminal
		ImageTaskStateManualReview,      // non-terminal (awaits operator, holds capacity)
		ImageTaskStateCompleted,         // terminal
		ImageTaskStateFailed,            // terminal
		ImageTaskStateCancelled,         // terminal
	}
	for i, s := range states {
		exec := &ImageTaskExecution{
			PublicTaskID:   fmt.Sprintf("imgtask_inflight_%d_%d", owner, i),
			TaskDBID:       int64(owner*1000 + i),
			OwnerUserID:    owner,
			Operation:      ImageTaskOperationGeneration,
			IdempotencyKey: fmt.Sprintf("inflight-key-%d-%d", owner, i),
			RequestHash:    "h",
			State:          s,
		}
		require.NoError(t, DB.Create(exec).Error)
	}
}
