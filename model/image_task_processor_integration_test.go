//go:build integration

package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListFairDueImageTaskExecutions_OnRealDB proves the owner-fair grouped
// query has identical semantics on MySQL 8 and PostgreSQL 16. In particular,
// a hot owner's rows cannot fill the bounded page and hide another due owner.
func TestListFairDueImageTaskExecutions_OnRealDB(t *testing.T) {
	setupWischoicerIntegrationDB(t)

	for i := 0; i < 60; i++ {
		exec := insertClaimableExecution(t, ImageTaskStateQueued, int64(i+1), 0, "")
		require.NoError(t, DB.Model(exec).Update("owner_user_id", 1).Error)
	}
	other := insertClaimableExecution(t, ImageTaskStateQueued, 1000, 0, "")
	require.NoError(t, DB.Model(other).Update("owner_user_id", 2).Error)

	execs, err := ListFairDueImageTaskExecutions(2000, 50, 3)
	require.NoError(t, err)
	require.Len(t, execs, 4)
	ownerCounts := map[int]int{}
	for _, exec := range execs {
		ownerCounts[exec.OwnerUserID]++
	}
	assert.Equal(t, 3, ownerCounts[1])
	assert.Equal(t, 1, ownerCounts[2])
}
