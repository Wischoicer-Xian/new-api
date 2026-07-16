//go:build integration

package model

import (
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateImageTaskAtomic_ConcurrentDifferentKeysConverges is the §6.1
// concurrency proof on real MySQL/PostgreSQL: with the cap pre-seeded to cap-1,
// two same-owner requests with different idempotency keys race; the owner fence
// serializes them so exactly one creates and exactly one is rejected at the cap,
// and the final in-flight count equals the cap. No sleep, no fake winner — all
// goroutine results are collected and asserted.
func TestCreateImageTaskAtomic_ConcurrentDifferentKeysConverges(t *testing.T) {
	setupWischoicerIntegrationDB(t)
	const owner = 7101
	require.NoError(t, DB.Create(&User{Id: owner, Username: "cc_owner_7101", AffCode: "ccaff_7101", Quota: 1000}).Error)
	prev := constant.MaxImageTasksPerUser
	constant.MaxImageTasksPerUser = 2
	t.Cleanup(func() { constant.MaxImageTasksPerUser = prev })

	// pre-seed cap-1 non-terminal execution
	require.NoError(t, DB.Create(&ImageTaskExecution{
		PublicTaskID: "imgtask_pre_7101", TaskDBID: 71001, OwnerUserID: owner,
		Operation: ImageTaskOperationGeneration, IdempotencyKey: "pre", RequestHash: "h",
		State: ImageTaskStateQueued, CreatedAt: 1, UpdatedAt: 1,
	}).Error)

	keys := []string{"conc-a", "conc-b"}
	const workers = 2
	start := make(chan struct{})
	var wg sync.WaitGroup
	type res struct {
		created bool
		err     error
	}
	results := make([]res, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			out, err := CreateImageTaskAtomic(ImageTaskCreateIntent{
				OwnerUserID:     owner,
				Group:           "default",
				Operation:       ImageTaskOperationGeneration,
				IdempotencyKey:  keys[i],
				RequestHash:     "h-" + keys[i],
				ReserveQuota:    5,
				BillingSnapshot: ImageTaskBillingSnapshot{OwnerUserID: owner, Group: "default", Operation: ImageTaskOperationGeneration, ReserveQuota: 5},
				Now:             17,
			})
			results[i] = res{created: out.Created, err: err}
		}()
	}
	close(start)
	wg.Wait()

	createdCount, capCount, otherCount := 0, 0, 0
	for _, r := range results {
		switch {
		case r.err == nil && r.created:
			createdCount++
		case r.err == ErrImageTaskInFlightCap:
			capCount++
		default:
			otherCount++
		}
	}
	assert.Equal(t, 1, createdCount, "exactly one new task under the cap")
	assert.Equal(t, 1, capCount, "exactly one rejected at the cap")
	assert.Zero(t, otherCount, "no unexpected errors")

	count, err := CountInFlightImageTasksByOwner(DB, owner)
	require.NoError(t, err)
	assert.Equal(t, int64(constant.MaxImageTasksPerUser), count, "final in-flight count equals the cap")
}

// TestCreateImageTaskAtomic_ConcurrentSameKeyReplays proves the same-key race:
// two same-owner requests with the same key+hash converge on the SAME task (one
// created, one replayed), occupy exactly one slot, and deduct the reserve once.
func TestCreateImageTaskAtomic_ConcurrentSameKeyReplays(t *testing.T) {
	setupWischoicerIntegrationDB(t)
	const owner = 7102
	require.NoError(t, DB.Create(&User{Id: owner, Username: "cc_owner_7102", AffCode: "ccaff_7102", Quota: 1000}).Error)
	prev := constant.MaxImageTasksPerUser
	constant.MaxImageTasksPerUser = 2
	t.Cleanup(func() { constant.MaxImageTasksPerUser = prev })

	const workers = 2
	start := make(chan struct{})
	var wg sync.WaitGroup
	type res struct {
		created bool
		execID  int64
		err     error
	}
	results := make([]res, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			out, err := CreateImageTaskAtomic(ImageTaskCreateIntent{
				OwnerUserID:     owner,
				Group:           "default",
				Operation:       ImageTaskOperationGeneration,
				IdempotencyKey:  "same-key",
				RequestHash:     "same-hash",
				ReserveQuota:    5,
				BillingSnapshot: ImageTaskBillingSnapshot{OwnerUserID: owner, Group: "default", Operation: ImageTaskOperationGeneration, ReserveQuota: 5},
				Now:             18,
			})
			eid := int64(0)
			if out.Execution != nil {
				eid = out.Execution.ID
			}
			results[i] = res{created: out.Created, execID: eid, err: err}
		}()
	}
	close(start)
	wg.Wait()

	for i, r := range results {
		require.NoError(t, r.err, "worker %d must not error", i)
	}
	assert.Equal(t, results[0].execID, results[1].execID, "both converge on the same task")
	createdCount := 0
	for _, r := range results {
		if r.created {
			createdCount++
		}
	}
	assert.Equal(t, 1, createdCount, "exactly one created, one replayed")

	count, err := CountInFlightImageTasksByOwner(DB, owner)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "same key occupies one slot")
	var user User
	require.NoError(t, DB.First(&user, owner).Error)
	assert.Equal(t, 995, user.Quota, "reserve deducted exactly once across the race")
}
