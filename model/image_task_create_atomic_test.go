package model

import (
	"encoding/json"
	"fmt"
	"sync"
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
		DB.Where("user_id = ?", owner).Delete(&Token{})
		DB.Where("user_id = ?", owner).Delete(&UserSubscription{})
		DB.Where("user_id = ?", owner).Delete(&Task{})
		DB.Where("id = ?", owner).Delete(&User{})
	})
}

// seedAtomicPlan inserts a minimal subscription plan and cleans it up.
func seedAtomicPlan(t *testing.T, id int) {
	t.Helper()
	require.NoError(t, DB.Create(&SubscriptionPlan{
		Id:           id,
		Title:        fmt.Sprintf("atomic_plan_%d", id),
		DurationUnit: "month",
	}).Error)
	t.Cleanup(func() { DB.Where("id = ?", id).Delete(&SubscriptionPlan{}) })
}

// seedAtomicSubscription inserts an active subscription with capacity. NextResetTime
// is set far in the future so maybeResetUserSubscriptionWithPlanTx is a no-op.
func seedAtomicSubscription(t *testing.T, subID, planID, userID int, total, used int64) {
	t.Helper()
	require.NoError(t, DB.Create(&UserSubscription{
		Id:            subID,
		UserId:        userID,
		PlanId:        planID,
		AmountTotal:   total,
		AmountUsed:    used,
		StartTime:     1_600_000_000,
		EndTime:       2_000_000_000,
		Status:        "active",
		NextResetTime: 2_000_000_000,
	}).Error)
}

func subAmountUsed(t *testing.T, id int) int64 {
	t.Helper()
	var s UserSubscription
	require.NoError(t, DB.Select("amount_used").First(&s, id).Error)
	return s.AmountUsed
}

// seedAtomicToken inserts a creation token for the owner and returns its id.
func seedAtomicToken(t *testing.T, id, userID, remain int, unlimited bool) {
	t.Helper()
	require.NoError(t, DB.Create(&Token{
		Id:             id,
		UserId:         userID,
		Name:           fmt.Sprintf("atomic_token_%d", id),
		Key:            fmt.Sprintf("sk-atomic-%d", id),
		Status:         1,
		RemainQuota:    remain,
		UnlimitedQuota: unlimited,
		ExpiredTime:    -1,
	}).Error)
}

func tokenRemain(t *testing.T, id int) int {
	t.Helper()
	var tk Token
	require.NoError(t, DB.Select("remain_quota").First(&tk, id).Error)
	return tk.RemainQuota
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
	// conflict does not trigger reserve: quota stays at the first create's deduction
	var user User
	require.NoError(t, DB.First(&user, owner).Error)
	assert.Equal(t, 95, user.Quota, "conflict must not reserve beyond the first create")
}

// TestCreateImageTaskAtomic_PreExistingKeyReplaysAtCap proves the lock-free
// first idempotency check wins over the cap: a request whose key already exists
// (even a terminal one) replays instead of being rejected at a full cap.
func TestCreateImageTaskAtomic_PreExistingKeyReplaysAtCap(t *testing.T) {
	const owner = 1006
	seedAtomicOwner(t, owner, 100)
	prevCap := constant.MaxImageTasksPerUser
	constant.MaxImageTasksPerUser = 1
	t.Cleanup(func() { constant.MaxImageTasksPerUser = prevCap })

	// fill the cap with a queued execution under a different key
	require.NoError(t, DB.Create(&ImageTaskExecution{
		PublicTaskID: "imgtask_fill_1006", TaskDBID: 6001, OwnerUserID: owner,
		Operation: ImageTaskOperationGeneration, IdempotencyKey: "filler", RequestHash: "h",
		State: ImageTaskStateQueued,
	}).Error)
	// a completed execution shares the request's key+hash; its task row must exist
	// for the replay to load it.
	require.NoError(t, DB.Create(&Task{ID: 6002, UserId: owner, TaskID: "task_done_1006"}).Error)
	require.NoError(t, DB.Create(&ImageTaskExecution{
		PublicTaskID: "imgtask_done_1006", TaskDBID: 6002, OwnerUserID: owner,
		Operation: ImageTaskOperationGeneration, IdempotencyKey: "replay-key", RequestHash: "h-replay",
		State: ImageTaskStateCompleted,
	}).Error)

	intent := baseAtomicIntent(owner, "replay-key")
	intent.RequestHash = "h-replay"
	out, err := CreateImageTaskAtomic(intent)
	require.NoError(t, err)
	assert.False(t, out.Created, "pre-existing key replays even when the cap is full")
	assert.Equal(t, "imgtask_done_1006", out.Execution.PublicTaskID)
}

// TestCreateImageTaskAtomic_ConcurrentSameKeyReplaysSQLite is the SQLite
// concurrency double-check: two goroutines invoking the same key concurrently
// converge on one created task and one replay, occupying one slot and
// deducting the reserve once. SQLite serializes the writes, but the goroutines
// run concurrently (start barrier) and exercise the double-checked idempotency
// guard, not a sequential single-call path.
func TestCreateImageTaskAtomic_ConcurrentSameKeyReplaysSQLite(t *testing.T) {
	const owner = 1007
	seedAtomicOwner(t, owner, 100)
	prevCap := constant.MaxImageTasksPerUser
	constant.MaxImageTasksPerUser = 5
	t.Cleanup(func() { constant.MaxImageTasksPerUser = prevCap })

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
		go func(idx int) {
			defer wg.Done()
			<-start
			out, err := CreateImageTaskAtomic(ImageTaskCreateIntent{
				OwnerUserID:     owner,
				Group:           "default",
				Operation:       ImageTaskOperationGeneration,
				IdempotencyKey:  "sqlite-same-key",
				RequestHash:     "sqlite-same-hash",
				ReserveQuota:    5,
				BillingSnapshot: snapshotJSON(map[string]int{"u": 5}),
				Now:             19,
			})
			eid := int64(0)
			if out.Execution != nil {
				eid = out.Execution.ID
			}
			results[idx] = res{created: out.Created, execID: eid, err: err}
		}(i)
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
	assert.Equal(t, int64(1), count)
	var user User
	require.NoError(t, DB.First(&user, owner).Error)
	assert.Equal(t, 95, user.Quota, "reserve deducted once across the race")
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

// P1-3 wallet + token matrix (subscription funding source is the remaining
// slice). Token-first then wallet, same transaction: failure of either rolls
// both back with no manual IncreaseTokenQuota.

func TestCreateImageTaskAtomic_LimitedTokenDeductedWithWallet(t *testing.T) {
	const owner = 1101
	seedAtomicOwner(t, owner, 100)
	seedAtomicToken(t, 1101, owner, 50, false)
	setCap(t, 5)

	intent := baseAtomicIntent(owner, "tok-limited")
	intent.TokenID = 1101
	out, err := CreateImageTaskAtomic(intent)
	require.NoError(t, err)
	require.True(t, out.Created)

	assert.Equal(t, 95, userQuota(t, owner), "wallet deducted")
	assert.Equal(t, 45, tokenRemain(t, 1101), "limited token deducted")
	assert.Equal(t, ImageTaskBillingSourceWallet, out.Task.PrivateData.BillingSource)
	assert.Equal(t, 1101, out.Task.PrivateData.TokenId)
}

func TestCreateImageTaskAtomic_UnlimitedTokenFrozenNotDeducted(t *testing.T) {
	const owner = 1102
	seedAtomicOwner(t, owner, 100)
	seedAtomicToken(t, 1102, owner, 999, true) // unlimited
	setCap(t, 5)

	intent := baseAtomicIntent(owner, "tok-unlimited")
	intent.TokenID = 1102
	out, err := CreateImageTaskAtomic(intent)
	require.NoError(t, err)
	require.True(t, out.Created)

	assert.Equal(t, 95, userQuota(t, owner), "wallet still deducted")
	assert.Equal(t, 999, tokenRemain(t, 1102), "unlimited token frozen, not deducted")
	assert.Equal(t, 1102, out.Task.PrivateData.TokenId)
}

func TestCreateImageTaskAtomic_TokenInsufficientRollsBack(t *testing.T) {
	const owner = 1103
	seedAtomicOwner(t, owner, 100)            // wallet sufficient
	seedAtomicToken(t, 1103, owner, 3, false) // token insufficient (<5)
	setCap(t, 5)

	intent := baseAtomicIntent(owner, "tok-insuff")
	intent.TokenID = 1103
	intent.ReserveQuota = 5
	_, err := CreateImageTaskAtomic(intent)
	assert.ErrorIs(t, err, ErrImageTaskInsufficientToken)

	// full rollback: wallet intact, token intact, no task/execution/ledger
	assert.Equal(t, 100, userQuota(t, owner))
	assert.Equal(t, 3, tokenRemain(t, 1103))
	count, _ := CountInFlightImageTasksByOwner(DB, owner)
	assert.Zero(t, count)
}

func TestCreateImageTaskAtomic_WalletInsufficientRollsBackTokenIntact(t *testing.T) {
	const owner = 1104
	seedAtomicOwner(t, owner, 3)                // wallet insufficient (<5)
	seedAtomicToken(t, 1104, owner, 100, false) // token sufficient
	setCap(t, 5)

	intent := baseAtomicIntent(owner, "wallet-insuff")
	intent.TokenID = 1104
	intent.ReserveQuota = 5
	_, err := CreateImageTaskAtomic(intent)
	assert.ErrorIs(t, err, ErrImageTaskInsufficientQuota)

	// token was deducted then rolled back with the transaction (no manual
	// IncreaseTokenQuota): both balances intact, nothing persisted.
	assert.Equal(t, 3, userQuota(t, owner))
	assert.Equal(t, 100, tokenRemain(t, 1104), "token deduction rolled back with the tx")
	count, _ := CountInFlightImageTasksByOwner(DB, owner)
	assert.Zero(t, count)
}

func setCap(t *testing.T, n int) {
	t.Helper()
	prev := constant.MaxImageTasksPerUser
	constant.MaxImageTasksPerUser = n
	t.Cleanup(func() { constant.MaxImageTasksPerUser = prev })
}

func userQuota(t *testing.T, owner int) int {
	t.Helper()
	var u User
	require.NoError(t, DB.First(&u, owner).Error)
	return u.Quota
}

// P1-1/P1-3 subscription funding source.

func TestCreateImageTaskAtomic_SubscriptionFundingDeducts(t *testing.T) {
	const owner = 1201
	seedAtomicOwner(t, owner, 100) // wallet present but must NOT be touched
	seedAtomicPlan(t, 1201)
	seedAtomicSubscription(t, 1201, 1201, owner, 100, 0)
	setCap(t, 5)

	intent := baseAtomicIntent(owner, "sub-1")
	intent.BillingPreference = ImageTaskBillingPrefSubscriptionOnly
	intent.ReserveQuota = 5
	out, err := CreateImageTaskAtomic(intent)
	require.NoError(t, err)
	require.True(t, out.Created)

	assert.Equal(t, int64(5), subAmountUsed(t, 1201), "subscription amount_used deducted")
	assert.Equal(t, 100, userQuota(t, owner), "wallet untouched on subscription path")
	assert.Equal(t, ImageTaskBillingSourceSubscription, out.Task.PrivateData.BillingSource)
	assert.Equal(t, 1201, out.Task.PrivateData.SubscriptionId)
}

func TestCreateImageTaskAtomic_SubscriptionInsufficientFallsBackToWallet(t *testing.T) {
	const owner = 1202
	seedAtomicOwner(t, owner, 100) // wallet fallback target
	seedAtomicPlan(t, 1202)
	seedAtomicSubscription(t, 1202, 1202, owner, 3, 0) // subscription capacity < reserve
	setCap(t, 5)

	// default = subscription_first: sub insufficient → fall back to wallet
	intent := baseAtomicIntent(owner, "sub-fallback")
	intent.ReserveQuota = 5
	out, err := CreateImageTaskAtomic(intent)
	require.NoError(t, err)
	require.True(t, out.Created)

	assert.Equal(t, int64(0), subAmountUsed(t, 1202), "subscription not consumed (insufficient)")
	assert.Equal(t, 95, userQuota(t, owner), "fell back to wallet")
	assert.Equal(t, ImageTaskBillingSourceWallet, out.Task.PrivateData.BillingSource)
}
