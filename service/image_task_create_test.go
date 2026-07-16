package service

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImageTaskCreateAllowed(t *testing.T) {
	prev := constant.ImageTaskCreateEnabled
	t.Cleanup(func() { constant.ImageTaskCreateEnabled = prev })

	constant.ImageTaskCreateEnabled = false
	assert.False(t, ImageTaskCreateAllowed(), "create is fail-closed by default (§14.1 placeholder off)")

	constant.ImageTaskCreateEnabled = true
	assert.True(t, ImageTaskCreateAllowed(), "create allowed when the switch is on")
}

// TestImageTaskInFlightStatusOf proves the per-user cap (§6.1): AtCap flips at
// the configured threshold and a cap <= 0 disables the limit.
func TestImageTaskInFlightStatusOf(t *testing.T) {
	truncate(t)

	prevCap := constant.MaxImageTasksPerUser
	constant.MaxImageTasksPerUser = 3
	t.Cleanup(func() { constant.MaxImageTasksPerUser = prevCap })

	mk := func(i int) {
		require.NoError(t, model.DB.Create(&model.ImageTaskExecution{
			PublicTaskID:   fmt.Sprintf("p3c_inflight_%d", i),
			TaskDBID:       int64(1000 + i),
			OwnerUserID:    1,
			Operation:      model.ImageTaskOperationGeneration,
			IdempotencyKey: fmt.Sprintf("p3c-key-%d", i),
			RequestHash:    "h",
			State:          model.ImageTaskStateQueued,
		}).Error)
	}

	mk(1)
	mk(2)
	status, err := ImageTaskInFlightStatusOf(1)
	require.NoError(t, err)
	assert.Equal(t, int64(2), status.Current)
	assert.Equal(t, 3, status.Cap)
	assert.False(t, status.AtCap, "under the cap is not at-cap")
	assert.Zero(t, status.RetryAfter)

	mk(3) // meets the cap
	status, err = ImageTaskInFlightStatusOf(1)
	require.NoError(t, err)
	assert.True(t, status.AtCap, "at the cap rejects with 429")
	assert.Equal(t, imageTaskInFlightRetryAfterSeconds, status.RetryAfter)

	// cap disabled
	constant.MaxImageTasksPerUser = 0
	status, err = ImageTaskInFlightStatusOf(1)
	require.NoError(t, err)
	assert.False(t, status.AtCap, "cap <= 0 disables the limit")
}
