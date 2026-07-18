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
	// ImageTaskCreateAllowed is the §14.1 route-level release/liveness gate: it
	// admits creation only when an operator has released it AND the processor is
	// live, so admitted work can advance. The matrix locks the four
	// create×processor combinations; only on/on releases the route.
	prevCreate := constant.ImageTaskCreateEnabled
	prevProc := constant.ImageTaskProcessorEnabled
	t.Cleanup(func() {
		constant.ImageTaskCreateEnabled = prevCreate
		constant.ImageTaskProcessorEnabled = prevProc
	})

	cases := []struct {
		name         string
		create, proc bool
		want         bool
	}{
		{"off/off fail-closed", false, false, false},
		{"off/on create not released", false, true, false},
		{"on/off processor not live", true, false, false},
		{"on/on releases the route", true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			constant.ImageTaskCreateEnabled = tc.create
			constant.ImageTaskProcessorEnabled = tc.proc
			assert.Equal(t, tc.want, ImageTaskCreateAllowed())
		})
	}
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
