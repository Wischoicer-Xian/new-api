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
	assert.False(t, ImageTaskCreateAllowed(), "a global switch cannot bypass the missing processor and allowlist")
}

// TestImageTaskInFlightStatusOf proves the READ-ONLY status primitive returns
// the user's non-terminal count and the configured cap. It deliberately does
// not assert any enforcement (AtCap/429): per §6.1 review, this primitive is
// not a concurrency-safe gate; enforcement lands in C3 inside one transaction.
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
}
