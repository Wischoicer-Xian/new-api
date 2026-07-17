package model

import (
	"context"
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFundMatrix_SQLite runs the same money-critical cases that the tagged
// MySQL/PostgreSQL suite invokes through runImageTaskCriticalFundCases.
func TestFundMatrix_SQLite(t *testing.T) {
	runImageTaskCriticalFundCases(t)
}

func runImageTaskCriticalFundCases(t *testing.T) {
	previousCap := constant.MaxImageTasksPerUser
	constant.MaxImageTasksPerUser = 10
	t.Cleanup(func() { constant.MaxImageTasksPerUser = previousCap })
	t.Cleanup(func() {
		var taskIDs []int64
		require.NoError(t, DB.Model(&Task{}).Where("user_id BETWEEN ? AND ?", 9801, 9807).Pluck("id", &taskIDs).Error)
		if len(taskIDs) > 0 {
			require.NoError(t, DB.Where("task_db_id IN ?", taskIDs).Delete(&TaskBillingLedger{}).Error)
			require.NoError(t, DB.Where("task_db_id IN ?", taskIDs).Delete(&ImageTaskExecution{}).Error)
		}
		require.NoError(t, DB.Where("user_id BETWEEN ? AND ?", 9801, 9807).Delete(&Task{}).Error)
		require.NoError(t, DB.Where("channel_id BETWEEN ? AND ?", 29801, 29807).Delete(&ChannelRevision{}).Error)
		require.NoError(t, DB.Where("id BETWEEN ? AND ?", 29801, 29807).Delete(&Channel{}).Error)
		require.NoError(t, DB.Unscoped().Where("id BETWEEN ? AND ?", 19801, 19807).Delete(&Token{}).Error)
		require.NoError(t, DB.Unscoped().Where("id BETWEEN ? AND ?", 9801, 9807).Delete(&User{}).Error)
	})

	t.Run("LimitedTokenAndSnapshot", func(t *testing.T) {
		owner, tokenID := 9801, 19801
		seedCriticalFundOwner(t, owner, tokenID, common.TokenStatusEnabled, -1, 100, false)
		out, err := reserveCriticalFundCase(owner, tokenID, "limited", criticalFundPrice(t, true))
		require.NoError(t, err)
		assert.Equal(t, FundingSourceWallet, out.FundingSource)
		var token Token
		require.NoError(t, DB.First(&token, tokenID).Error)
		assert.Equal(t, 58, token.RemainQuota)
		assert.Equal(t, 42, token.UsedQuota)
		assert.Equal(t, int64(1_700_000_000), token.AccessedTime)
		var ledger TaskBillingLedger
		require.NoError(t, DB.Where("task_db_id = ? AND stage = ?", out.Task.ID, TaskBillingReserve).First(&ledger).Error)
		assert.Equal(t, BillingStateApplied, ledger.State)
		assert.Equal(t, 42, ledger.QuotaAmount)
		var snapshot ImageTaskBillingSnapshotV1
		require.NoError(t, common.Unmarshal(ledger.BillingSnapshot, &snapshot))
		require.NoError(t, snapshot.Validate())
		assert.Equal(t, owner, snapshot.OwnerUserID)
		assert.Equal(t, tokenID, snapshot.CreationTokenID)
		assert.Equal(t, out.Execution.ChannelRevisionID, snapshot.ChannelRevisionID)
		assert.Equal(t, FundingSourceWallet, snapshot.FundingSource)
	})

	t.Run("UnlimitedToken", func(t *testing.T) {
		owner, tokenID := 9802, 19802
		seedCriticalFundOwner(t, owner, tokenID, common.TokenStatusEnabled, -1, 1, true)
		_, err := reserveCriticalFundCase(owner, tokenID, "unlimited", criticalFundPrice(t, true))
		require.NoError(t, err)
		var token Token
		require.NoError(t, DB.First(&token, tokenID).Error)
		assert.Equal(t, 1, token.RemainQuota)
		assert.Zero(t, token.UsedQuota)
	})

	for _, tc := range []struct {
		name      string
		owner     int
		tokenID   int
		status    int
		expiredAt int64
		remain    int
	}{
		{name: "DisabledToken", owner: 9803, tokenID: 19803, status: common.TokenStatusDisabled, expiredAt: -1, remain: 100},
		{name: "ExpiredToken", owner: 9804, tokenID: 19804, status: common.TokenStatusEnabled, expiredAt: 1_699_999_999, remain: 100},
		{name: "InsufficientTokenRollsBack", owner: 9805, tokenID: 19805, status: common.TokenStatusEnabled, expiredAt: -1, remain: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedCriticalFundOwner(t, tc.owner, tc.tokenID, tc.status, tc.expiredAt, tc.remain, false)
			_, err := reserveCriticalFundCase(tc.owner, tc.tokenID, tc.name, criticalFundPrice(t, true))
			assert.ErrorIs(t, err, ErrImageTaskTokenInvalid)
			var taskCount, ledgerCount int64
			require.NoError(t, DB.Model(&Task{}).Where("user_id = ?", tc.owner).Count(&taskCount).Error)
			require.NoError(t, DB.Model(&TaskBillingLedger{}).
				Joins("JOIN tasks ON tasks.id = task_billing_ledgers.task_db_id").
				Where("tasks.user_id = ?", tc.owner).Count(&ledgerCount).Error)
			assert.Zero(t, taskCount)
			assert.Zero(t, ledgerCount)
			var user User
			require.NoError(t, DB.First(&user, tc.owner).Error)
			assert.Equal(t, 100, user.Quota)
		})
	}

	t.Run("DifferentKeyCap", func(t *testing.T) {
		owner, tokenID := 9806, 19806
		seedCriticalFundOwner(t, owner, tokenID, common.TokenStatusEnabled, -1, 1000, false)
		constant.MaxImageTasksPerUser = 1
		_, err := reserveCriticalFundCase(owner, tokenID, "cap-first", criticalFundPrice(t, true))
		require.NoError(t, err)
		_, err = reserveCriticalFundCase(owner, tokenID, "cap-second", criticalFundPrice(t, true))
		assert.ErrorIs(t, err, ErrImageTaskInFlightCapReached)
	})

	t.Run("ZeroQuotaFreeModel", func(t *testing.T) {
		owner, tokenID := 9807, 19807
		seedCriticalFundOwner(t, owner, tokenID, common.TokenStatusEnabled, -1, 100, false)
		out, err := reserveCriticalFundCase(owner, tokenID, "free", criticalFundPrice(t, false))
		require.NoError(t, err)
		assert.Equal(t, FundingSourceFree, out.FundingSource)
		assert.Zero(t, out.AppliedReserveQuota)
	})
}

func seedCriticalFundOwner(t *testing.T, owner, tokenID, status int, expiredAt int64, remain int, unlimited bool) {
	t.Helper()
	user := User{Id: owner, Username: fmt.Sprintf("fund-%d", owner), AffCode: fmt.Sprintf("fund-aff-%d", owner), Quota: 100, Status: common.UserStatusEnabled}
	setting := user.GetSetting()
	setting.BillingPreference = "wallet_only"
	user.SetSetting(setting)
	require.NoError(t, DB.Create(&user).Error)
	require.NoError(t, DB.Create(&Token{
		Id: tokenID, UserId: owner, Key: fmt.Sprintf("sk-fund-%d", owner), Name: "fund",
		Status: status, ExpiredTime: expiredAt, RemainQuota: remain, UnlimitedQuota: unlimited,
	}).Error)
	channelID := owner + 20000
	require.NoError(t, DB.Create(&Channel{Id: channelID, Name: "fund-image", Status: common.ChannelStatusEnabled}).Error)
	require.NoError(t, DB.Create(&ChannelRevision{
		ChannelID: channelID, RevisionNumber: 1, AdapterVersion: "integration/v1",
		Settings: []byte(`{"schema_version":1,"execution_config":"{\\"defaults\\":{\\"generation\\":\\"sync\\"}}"}`),
	}).Error)
}

func criticalFundPrice(t *testing.T, charged bool) *ImageTaskPriceResolution {
	t.Helper()
	price := 0.0
	freePreconsume := false
	if charged {
		price = 0.000085
		freePreconsume = true
	}
	value, err := NewImageTaskPriceResolution(
		"model_price", "model_price", "img-v1", "img-v1", "default",
		price, 0, 1, 500000, freePreconsume, nil,
	)
	require.NoError(t, err)
	return value
}

func reserveCriticalFundCase(owner, tokenID int, key string, price *ImageTaskPriceResolution) (ImageTaskReserveOutcome, error) {
	var revision ChannelRevision
	if err := DB.Where("channel_id = ?", owner+20000).First(&revision).Error; err != nil {
		return ImageTaskReserveOutcome{}, err
	}
	return ReserveImageTask(context.Background(), ImageTaskReserveCommand{
		OwnerUserID: owner, CreationTokenID: tokenID,
		Operation: ImageTaskOperationGeneration, IdempotencyKey: key, RequestHash: "hash-" + key,
		ChannelRevisionID: revision.ID, ExecutionMode: "sync", AdapterVersion: revision.AdapterVersion,
		Price: price, RequestData: []byte(`{"model":"img-v1","prompt":"fund matrix"}`), Now: 1_700_000_000,
	})
}
