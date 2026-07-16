//go:build integration

package model

import (
	"strconv"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyVideoPlatform is the numeric-string platform of a registered video
// channel type, which is what production video tasks carry.
var legacyVideoPlatformIT = constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeKling))

// TestPollerIsolation_AllowlistExcludesImageBeforeLimit proves the §7.3 fix on
// each real database: the platform allowlist is applied in the WHERE clause,
// BEFORE ORDER/LIMIT, so image tasks seeded ahead of a legacy task cannot fill
// the limit window and starve the legacy task out of the poller's view. A
// post-limit denylist would return only the image rows here; the pre-limit
// allowlist returns the legacy row and no image row.
func TestPollerIsolation_AllowlistExcludesImageBeforeLimit(t *testing.T) {
	setupWischoicerIntegrationDB(t)

	now := time.Now().Unix()
	mk := func(taskID string, platform constant.TaskPlatform) *Task {
		return &Task{TaskID: taskID, Platform: platform, UserId: 1, Action: "generate", Status: TaskStatusInProgress, Progress: "30%", SubmitTime: now, CreatedAt: now, UpdatedAt: now}
	}
	// Seed image tasks first (lower ids) so they would own the whole window
	// under a post-limit filter, then the single legacy task.
	for i := 0; i < 5; i++ {
		require.NoError(t, DB.Create(mk("img-"+itoa(i), constant.TaskPlatformWischoicerImage)).Error)
	}
	require.NoError(t, DB.Create(mk("legacy-1", legacyVideoPlatformIT)).Error)

	fetched := GetAllUnFinishSyncTasks(3) // window smaller than the image backlog
	require.Len(t, fetched, 1, "image rows are excluded before LIMIT; only the legacy task is visible — no starvation")
	assert.Equal(t, "legacy-1", fetched[0].TaskID)
}

// TestPollerIsolation_HasUnfinishedSyncTasksRespectsAllowlist proves the
// async_task_poll existence gate honors the allowlist: image-only load does not
// spin up the legacy poller, while a legacy task does.
func TestPollerIsolation_HasUnfinishedSyncTasksRespectsAllowlist(t *testing.T) {
	setupWischoicerIntegrationDB(t)

	now := time.Now().Unix()
	mk := func(taskID string, platform constant.TaskPlatform) *Task {
		return &Task{TaskID: taskID, Platform: platform, UserId: 1, Action: "generate", Status: TaskStatusInProgress, Progress: "30%", SubmitTime: now, CreatedAt: now, UpdatedAt: now}
	}

	// image-only: gate must be false (no legacy work pending)
	require.NoError(t, DB.Create(mk("img-only", constant.TaskPlatformWischoicerImage)).Error)
	assert.False(t, HasUnfinishedSyncTasks(), "image-only load must not trigger the legacy async_task_poll")

	// add one legacy task: gate must be true
	require.NoError(t, DB.Create(mk("legacy-only", legacyVideoPlatformIT)).Error)
	assert.True(t, HasUnfinishedSyncTasks(), "a legacy task pending must trigger the legacy async_task_poll")
}

// TestPollerIsolation_TimedOutQueryExcludesImage proves the timeout sweep query
// applies the same pre-limit allowlist, so timed-out image tasks never reach
// the legacy sweep (and cannot strand legacy timeouts behind them).
func TestPollerIsolation_TimedOutQueryExcludesImage(t *testing.T) {
	setupWischoicerIntegrationDB(t)

	old := time.Now().Unix() - 7200
	mk := func(taskID string, platform constant.TaskPlatform) *Task {
		return &Task{TaskID: taskID, Platform: platform, UserId: 1, Action: "generate", Status: TaskStatusInProgress, Progress: "30%", SubmitTime: old, CreatedAt: old, UpdatedAt: old}
	}
	for i := 0; i < 3; i++ {
		require.NoError(t, DB.Create(mk("to-img-"+itoa(i), constant.TaskPlatformWischoicerImage)).Error)
	}
	require.NoError(t, DB.Create(mk("to-legacy", legacyVideoPlatformIT)).Error)

	timedOut := GetTimedOutUnfinishedTasks(time.Now().Unix()-60, 100)
	require.Len(t, timedOut, 1, "only the legacy timed-out task is returned; image timed-out tasks are excluded before LIMIT")
	assert.Equal(t, "to-legacy", timedOut[0].TaskID)
}

// TestPollerIsolation_TimedOutQueryIncludesHistoricalAliases is the backward-
// compatibility characterization (§7.3 + b3c4d972): timed-out rows carrying the
// pre-migration named-string platforms kling/jimeng are still returned by the
// timeout query so the sweep can converge them to failure, while wis_image and
// unknown platforms stay excluded.
func TestPollerIsolation_TimedOutQueryIncludesHistoricalAliases(t *testing.T) {
	setupWischoicerIntegrationDB(t)

	old := time.Now().Unix() - 7200
	mk := func(taskID string, platform constant.TaskPlatform) *Task {
		return &Task{TaskID: taskID, Platform: platform, UserId: 1, Action: "generate", Status: TaskStatusInProgress, Progress: "30%", SubmitTime: old, CreatedAt: old, UpdatedAt: old}
	}
	require.NoError(t, DB.Create(mk("to-kling", constant.TaskPlatform("kling"))).Error)
	require.NoError(t, DB.Create(mk("to-jimeng", constant.TaskPlatform("jimeng"))).Error)
	require.NoError(t, DB.Create(mk("to-image", constant.TaskPlatformWischoicerImage)).Error)
	require.NoError(t, DB.Create(mk("to-unknown", constant.TaskPlatform("999"))).Error)

	timedOut := GetTimedOutUnfinishedTasks(time.Now().Unix()-60, 100)
	ids := make(map[string]bool, len(timedOut))
	for _, tk := range timedOut {
		ids[tk.TaskID] = true
	}
	assert.True(t, ids["to-kling"], "historical kling timed-out row is returned for convergence")
	assert.True(t, ids["to-jimeng"], "historical jimeng timed-out row is returned for convergence")
	assert.False(t, ids["to-image"], "image timed-out row stays excluded")
	assert.False(t, ids["to-unknown"], "unknown timed-out row stays excluded")
}

// TestPollerIsolation_HistoricalAliasesNotPolled proves the two-domain split:
// historical named-string rows (kling/jimeng) have no provider adaptor, so they
// are excluded from the polling fetch even though they remain in the timeout
// convergence set. Only currently-pollable legacy platforms are polled.
func TestPollerIsolation_HistoricalAliasesNotPolled(t *testing.T) {
	setupWischoicerIntegrationDB(t)

	now := time.Now().Unix()
	mk := func(taskID string, platform constant.TaskPlatform) *Task {
		return &Task{TaskID: taskID, Platform: platform, UserId: 1, Action: "generate", Status: TaskStatusInProgress, Progress: "30%", SubmitTime: now, CreatedAt: now, UpdatedAt: now}
	}
	require.NoError(t, DB.Create(mk("named-kling", constant.TaskPlatform("kling"))).Error)
	require.NoError(t, DB.Create(mk("named-jimeng", constant.TaskPlatform("jimeng"))).Error)
	require.NoError(t, DB.Create(mk("numeric-legacy", legacyVideoPlatformIT)).Error)

	fetched := GetAllUnFinishSyncTasks(100)
	require.Len(t, fetched, 1, "historical named-string rows are not pollable; only the numeric legacy task is")
	assert.Equal(t, "numeric-legacy", fetched[0].TaskID)
}

// itoa is a local helper to keep the test bodies free of strconv noise.
func itoa(i int) string { return strconv.Itoa(i) }
