//go:build integration

package model

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPollerIsolation_ImageTasksExcludedOnRealDB proves on each real database
// that the legacy unfinished-task fetch returns image-platform Task rows — so
// they ARE in the legacy poller's input set and would leak into
// UpdateVideoTasks without the §7.3 carve-out — and that the
// constant.IsImageTaskPlatform predicate removes exactly those rows while
// keeping every legacy platform.
func TestPollerIsolation_ImageTasksExcludedOnRealDB(t *testing.T) {
	setupWischoicerIntegrationDB(t)

	now := time.Now().Unix()
	rows := []*Task{
		{TaskID: "video-1", Platform: constant.TaskPlatform("kling"), UserId: 1, Action: "generate", Status: TaskStatusInProgress, Progress: "30%", SubmitTime: now, CreatedAt: now, UpdatedAt: now},
		{TaskID: "suno-1", Platform: constant.TaskPlatformSuno, UserId: 1, Action: "generate", Status: TaskStatusInProgress, Progress: "30%", SubmitTime: now, CreatedAt: now, UpdatedAt: now},
		{TaskID: "image-1", Platform: constant.TaskPlatformWischoicerImage, UserId: 1, Action: "generate", Status: TaskStatusInProgress, Progress: "30%", SubmitTime: now, CreatedAt: now, UpdatedAt: now},
		{TaskID: "image-2", Platform: constant.TaskPlatformWischoicerImage, UserId: 1, Action: "generate", Status: TaskStatusInProgress, Progress: "30%", SubmitTime: now, CreatedAt: now, UpdatedAt: now},
	}
	for _, r := range rows {
		require.NoError(t, DB.Create(r).Error)
	}

	fetched := GetAllUnFinishSyncTasks(100)
	require.Len(t, fetched, 4, "all four unfinished tasks are returned; image tasks are in the legacy fetch set")

	legacyCount, imageCount := 0, 0
	for _, task := range fetched {
		if constant.IsImageTaskPlatform(task.Platform) {
			imageCount++
		} else {
			legacyCount++
		}
	}
	assert.Equal(t, 2, legacyCount, "legacy video + suno tasks are kept by the predicate")
	assert.Equal(t, 2, imageCount, "image tasks are present but identified for exclusion")
}

// TestPollerIsolation_TimedOutImageTasksExcludedOnRealDB proves the timeout
// sweep query also returns timed-out image tasks (the sweep leak surface) and
// the same predicate excludes them, so the legacy sweep skips image tasks on
// each database.
func TestPollerIsolation_TimedOutImageTasksExcludedOnRealDB(t *testing.T) {
	setupWischoicerIntegrationDB(t)

	old := time.Now().Unix() - 7200
	rows := []*Task{
		{TaskID: "to-video", Platform: constant.TaskPlatform("kling"), UserId: 1, Action: "generate", Status: TaskStatusInProgress, Progress: "30%", SubmitTime: old, CreatedAt: old, UpdatedAt: old},
		{TaskID: "to-image", Platform: constant.TaskPlatformWischoicerImage, UserId: 1, Action: "generate", Status: TaskStatusInProgress, Progress: "30%", SubmitTime: old, CreatedAt: old, UpdatedAt: old},
	}
	for _, r := range rows {
		require.NoError(t, DB.Create(r).Error)
	}

	timedOut := GetTimedOutUnfinishedTasks(time.Now().Unix()-60, 100)
	require.Len(t, timedOut, 2, "both timed-out tasks are returned; image is in the sweep fetch set")

	imageInSweep := 0
	for _, task := range timedOut {
		if constant.IsImageTaskPlatform(task.Platform) {
			imageInSweep++
		}
	}
	assert.Equal(t, 1, imageInSweep, "image task is present in the sweep fetch but skipped by the predicate")
}
