package model

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedAtomicOwner inserts a user with the given quota and returns its id.
func seedAtomicOwner(t *testing.T, id int, quota int) {
	t.Helper()
	require.NoError(t, DB.Create(&User{
		Id:       id,
		Username: fmt.Sprintf("atomic_owner_%d", id),
		AffCode:  fmt.Sprintf("aff_%d", id),
		Quota:    quota,
	}).Error)
	cleanupAtomicOwner(t, id)
}

// cleanupAtomicOwner removes every row the atomic-create tests seed for one
// owner (ledger, execution, task, user). The model package shares one DB across
// all tests, so leaving rows pollutes later foundation tests (unique-index
// collisions surface as OnConflict-no-row "record not found").
func cleanupAtomicOwner(t *testing.T, owner int) {
	t.Helper()
	t.Cleanup(func() {
		var taskIDs []int64
		DB.Model(&Task{}).Where("user_id = ?", owner).Pluck("id", &taskIDs)
		if len(taskIDs) > 0 {
			DB.Where("task_db_id IN ?", taskIDs).Delete(&TaskBillingLedger{})
		}
		DB.Where("owner_user_id = ?", owner).Delete(&ImageTaskExecution{})
		DB.Where("user_id = ?", owner).Delete(&Task{})
		DB.Where("id = ?", owner).Delete(&User{})
	})
}

func baseAtomicIntent(owner int, key string) ImageTaskCreateIntent {
	return ImageTaskCreateIntent{
		OwnerUserID:     owner,
		Group:           "default",
		Operation:       ImageTaskOperationGeneration,
		IdempotencyKey:  key,
		RequestHash:     "hash-" + key,
		ReserveQuota:    5,
		BillingSnapshot: snapshotJSON(map[string]int{"unit": 5}),
		Now:             1_700_000_000,
	}
}

// snapshotJSON builds a billing-snapshot fixture. The input is a static map, so
// the marshal error is not asserted; common.Marshal on a map cannot fail.
func snapshotJSON(v any) json.RawMessage {
	b, _ := common.Marshal(v)
	return b
}

func TestCreateImageTaskAtomic_CreatesNewTaskAndReserves(t *testing.T) {
	const owner = 1001
	seedAtomicOwner(t, owner, 100)
	prevCap := constant.MaxImageTasksPerUser
	constant.MaxImageTasksPerUser = 5
	t.Cleanup(func() { constant.MaxImageTasksPerUser = prevCap })

	out, err := CreateImageTaskAtomic(baseAtomicIntent(owner, "key-1"))
	require.NoError(t, err)
	require.True(t, out.Created)
	require.NotNil(t, out.Task)
	require.NotNil(t, out.Execution)
	assert.Equal(t, constant.TaskPlatform(constant.TaskPlatformWischoicerImage), out.Task.Platform)
	assert.Equal(t, ImageTaskStateQueued, out.Execution.State)

	// reserve ledger applied + quota deducted by the reserve amount
	var user User
	require.NoError(t, DB.First(&user, owner).Error)
	assert.Equal(t, 95, user.Quota, "reserve deducted")
	var ledger TaskBillingLedger
	require.NoError(t, DB.Where("task_db_id = ?", out.Task.ID).First(&ledger).Error)
	assert.Equal(t, TaskBillingReserve, ledger.Stage)
	assert.Equal(t, BillingStateApplied, ledger.State)

	count, err := CountInFlightImageTasksByOwner(DB, owner)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
}

func TestCreateImageTaskAtomic_ReplaysSameKeyHash(t *testing.T) {
	const owner = 1002
	seedAtomicOwner(t, owner, 100)
	prevCap := constant.MaxImageTasksPerUser
	constant.MaxImageTasksPerUser = 5
	t.Cleanup(func() { constant.MaxImageTasksPerUser = prevCap })

	first, err := CreateImageTaskAtomic(baseAtomicIntent(owner, "rep-1"))
	require.NoError(t, err)
	require.True(t, first.Created)

	// same key + same hash: replay, no new task, no second deduction
	second, err := CreateImageTaskAtomic(baseAtomicIntent(owner, "rep-1"))
	require.NoError(t, err)
	assert.False(t, second.Created, "same key+hash replays")
	assert.Equal(t, first.Task.ID, second.Task.ID)
	assert.Equal(t, first.Execution.ID, second.Execution.ID)

	var user User
	require.NoError(t, DB.First(&user, owner).Error)
	assert.Equal(t, 95, user.Quota, "replay does not double-deduct")
}

func TestCreateImageTaskAtomic_ConflictOnSameKeyDifferentHash(t *testing.T) {
	const owner = 1003
	seedAtomicOwner(t, owner, 100)
	prevCap := constant.MaxImageTasksPerUser
	constant.MaxImageTasksPerUser = 5
	t.Cleanup(func() { constant.MaxImageTasksPerUser = prevCap })

	_, err := CreateImageTaskAtomic(baseAtomicIntent(owner, "cf-1"))
	require.NoError(t, err)

	intent := baseAtomicIntent(owner, "cf-1")
	intent.RequestHash = "different-hash"
	_, err = CreateImageTaskAtomic(intent)
	assert.ErrorIs(t, err, ErrImageTaskIdempotencyConflict)

	// no extra slot consumed
	count, err := CountInFlightImageTasksByOwner(DB, owner)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
}

func TestCreateImageTaskAtomic_RejectsAtCap(t *testing.T) {
	const owner = 1004
	seedAtomicOwner(t, owner, 100)
	prevCap := constant.MaxImageTasksPerUser
	constant.MaxImageTasksPerUser = 2
	t.Cleanup(func() { constant.MaxImageTasksPerUser = prevCap })

	// fill the cap with two non-terminal executions
	require.NoError(t, DB.Create(&ImageTaskExecution{
		PublicTaskID: "imgtask_cap_1", TaskDBID: 2001, OwnerUserID: owner,
		Operation: ImageTaskOperationGeneration, IdempotencyKey: "cap-a", RequestHash: "h",
		State: ImageTaskStateQueued,
	}).Error)
	require.NoError(t, DB.Create(&ImageTaskExecution{
		PublicTaskID: "imgtask_cap_2", TaskDBID: 2002, OwnerUserID: owner,
		Operation: ImageTaskOperationGeneration, IdempotencyKey: "cap-b", RequestHash: "h",
		State: ImageTaskStateQueued,
	}).Error)

	_, err := CreateImageTaskAtomic(baseAtomicIntent(owner, "cap-c"))
	assert.ErrorIs(t, err, ErrImageTaskInFlightCap)

	// no task/ledger created, quota untouched
	var n int64
	DB.Model(&ImageTaskExecution{}).Where("owner_user_id = ?", owner).Count(&n)
	assert.Equal(t, int64(2), n, "cap rejection creates nothing")
	var user User
	require.NoError(t, DB.First(&user, owner).Error)
	assert.Equal(t, 100, user.Quota, "cap rejection does not reserve")
}

func TestCreateImageTaskAtomic_RejectsInsufficientQuota(t *testing.T) {
	const owner = 1005
	seedAtomicOwner(t, owner, 3) // less than reserve
	prevCap := constant.MaxImageTasksPerUser
	constant.MaxImageTasksPerUser = 5
	t.Cleanup(func() { constant.MaxImageTasksPerUser = prevCap })

	intent := baseAtomicIntent(owner, "iq-1")
	intent.ReserveQuota = 5
	_, err := CreateImageTaskAtomic(intent)
	assert.ErrorIs(t, err, ErrImageTaskInsufficientQuota)

	// nothing persisted: no task, no execution, quota intact, no ledger applied
	count, err := CountInFlightImageTasksByOwner(DB, owner)
	require.NoError(t, err)
	assert.Zero(t, count)
	var user User
	require.NoError(t, DB.First(&user, owner).Error)
	assert.Equal(t, 3, user.Quota, "insufficient quota leaves balance intact")
}
