//go:build integration

package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCountInFlightImageTasksByOwner_OnRealDB proves the §6.1 in-flight count
// query behaves identically on real MySQL/PostgreSQL: terminal states free
// capacity, every non-terminal state (incl. manual_review, submission_unknown)
// holds it.
func TestCountInFlightImageTasksByOwner_OnRealDB(t *testing.T) {
	setupWischoicerIntegrationDB(t)
	seedInFlightRows(t, 7)

	count, err := CountInFlightImageTasksByOwner(DB, 7)
	require.NoError(t, err)
	assert.Equal(t, int64(6), count, "six non-terminal states count; three terminal states do not")
}
