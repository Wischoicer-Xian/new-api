package service

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyVideoPlatform is a real legacy-polling platform value: the numeric
// string of a registered video channel type, which is what production video
// tasks carry (relay.GetTaskPlatform -> strconv.Itoa(channelType)).
var legacyVideoPlatform = constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeKling))

// TestFilterLegacyPollingTasks is the pure invariant for the in-memory secondary
// guard: it keeps Suno + registered video numeric platforms and drops image,
// Midjourney (own poller), and any unknown/empty platform.
func TestFilterLegacyPollingTasks(t *testing.T) {
	tasks := []*model.Task{
		{TaskID: "video-1", Platform: legacyVideoPlatform},
		{TaskID: "suno-1", Platform: constant.TaskPlatformSuno},
		{TaskID: "image-1", Platform: constant.TaskPlatformWischoicerImage},
		{TaskID: "mj-1", Platform: constant.TaskPlatformMidjourney},
		{TaskID: "unknown-1", Platform: constant.TaskPlatform("999")},
		{TaskID: "empty-1", Platform: constant.TaskPlatform("")},
	}
	legacy, dropped := filterLegacyPollingTasks(tasks)

	assert.Equal(t, 4, dropped, "image, mj, unknown, empty are dropped")
	require.Len(t, legacy, 2)
	got := []string{legacy[0].TaskID, legacy[1].TaskID}
	assert.Equal(t, []string{"video-1", "suno-1"}, got)
}

// TestRunTaskPollingOnceKeepsOnlyAllowlisted proves the §7.3 SQL allowlist is
// applied before LIMIT at the query layer: with image and unknown-platform tasks
// seeded alongside a legacy video task, only the legacy task is visible to the
// poller (UnfinishedTasks=1), so image/unknown can never be dispatched even when
// they would otherwise fill the window. No starvation.
func TestRunTaskPollingOnceKeepsOnlyAllowlisted(t *testing.T) {
	truncate(t)

	prevLimit := constant.TaskQueryLimit
	constant.TaskQueryLimit = 100
	t.Cleanup(func() { constant.TaskQueryLimit = prevLimit })

	now := time.Now().Unix()
	seed := func(taskID string, platform constant.TaskPlatform) {
		require.NoError(t, model.DB.Create(&model.Task{
			TaskID:     taskID,
			Platform:   platform,
			UserId:     1,
			Action:     constant.TaskActionGenerate,
			Status:     model.TaskStatusInProgress,
			Progress:   "30%",
			SubmitTime: now,
			CreatedAt:  now,
			UpdatedAt:  now,
		}).Error)
	}
	// Seed image and unknown FIRST so they would fill the limit window if the
	// filter were post-limit; then the legacy task.
	seed("img-a", constant.TaskPlatformWischoicerImage)
	seed("img-b", constant.TaskPlatformWischoicerImage)
	seed("unknown-a", constant.TaskPlatform("999"))
	seed("legacy-a", legacyVideoPlatform)

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	summary := RunTaskPollingOnce(ctx, nil)

	assert.Equal(t, 1, summary.UnfinishedTasks, "only the legacy task survives the SQL allowlist; image/unknown are excluded before LIMIT")
	assert.Equal(t, 0, summary.NonLegacyTasksSkipped, "the secondary guard sees only the already-filtered legacy task")
}

// TestSweepTimedOutTasksKeepsOnlyAllowlisted proves the timeout sweep query
// applies the same allowlist, so a timed-out legacy task is swept while timed-
// out image and unknown tasks are left untouched (they own their own timeout
// path). The fixed limit=100 cannot strand legacy timeouts behind image rows.
func TestSweepTimedOutTasksKeepsOnlyAllowlisted(t *testing.T) {
	truncate(t)

	prevTimeout := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 1
	t.Cleanup(func() { constant.TaskTimeoutMinutes = prevTimeout })

	old := time.Now().Unix() - 7200
	seed := func(taskID string, platform constant.TaskPlatform) *model.Task {
		tk := &model.Task{
			TaskID:     taskID,
			Platform:   platform,
			UserId:     1,
			Action:     constant.TaskActionGenerate,
			Status:     model.TaskStatusInProgress,
			Progress:   "30%",
			Quota:      0,
			SubmitTime: old,
			CreatedAt:  old,
			UpdatedAt:  old,
		}
		require.NoError(t, model.DB.Create(tk).Error)
		return tk
	}
	legacyTask := seed("to-legacy", legacyVideoPlatform)
	imageTask := seed("to-image", constant.TaskPlatformWischoicerImage)
	unknownTask := seed("to-unknown", constant.TaskPlatform("999"))

	sweepTimedOutTasks(context.Background())

	var legacyAfter, imageAfter, unknownAfter model.Task
	require.NoError(t, model.DB.First(&legacyAfter, legacyTask.ID).Error)
	require.NoError(t, model.DB.First(&imageAfter, imageTask.ID).Error)
	require.NoError(t, model.DB.First(&unknownAfter, unknownTask.ID).Error)

	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), legacyAfter.Status, "legacy timed-out task is swept")
	assert.Equal(t, model.TaskStatus(model.TaskStatusInProgress), imageAfter.Status, "image task is not swept by the legacy timeout")
	assert.Equal(t, model.TaskStatus(model.TaskStatusInProgress), unknownAfter.Status, "unknown platform is not swept by the legacy timeout")
}

// TestSweepTimedOutTasksConvergesHistoricalAliases is the §7.3 + backward-
// compatibility characterization: pre-b3c4d972 rows with the named-string
// platforms kling/jimeng have no provider adaptor (so they are not polled) but
// the timeout sweep still owes them failure/finalize. The sweep must converge
// them while leaving image/unknown untouched.
func TestSweepTimedOutTasksConvergesHistoricalAliases(t *testing.T) {
	truncate(t)

	prevTimeout := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 1
	t.Cleanup(func() { constant.TaskTimeoutMinutes = prevTimeout })

	old := time.Now().Unix() - 7200
	seed := func(taskID string, platform constant.TaskPlatform) *model.Task {
		tk := &model.Task{
			TaskID: taskID, Platform: platform, UserId: 1, Action: constant.TaskActionGenerate,
			Status: model.TaskStatusInProgress, Progress: "30%", Quota: 0,
			SubmitTime: old, CreatedAt: old, UpdatedAt: old,
		}
		require.NoError(t, model.DB.Create(tk).Error)
		return tk
	}
	kling := seed("to-kling-named", constant.TaskPlatform("kling"))
	jimeng := seed("to-jimeng-named", constant.TaskPlatform("jimeng"))
	imageTask := seed("to-image", constant.TaskPlatformWischoicerImage)
	unknownTask := seed("to-unknown", constant.TaskPlatform("999"))

	sweepTimedOutTasks(context.Background())

	var klingAfter, jimengAfter, imageAfter, unknownAfter model.Task
	require.NoError(t, model.DB.First(&klingAfter, kling.ID).Error)
	require.NoError(t, model.DB.First(&jimengAfter, jimeng.ID).Error)
	require.NoError(t, model.DB.First(&imageAfter, imageTask.ID).Error)
	require.NoError(t, model.DB.First(&unknownAfter, unknownTask.ID).Error)

	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), klingAfter.Status, "historical kling row is swept to failure")
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), jimengAfter.Status, "historical jimeng row is swept to failure")
	assert.Equal(t, model.TaskStatus(model.TaskStatusInProgress), imageAfter.Status, "image is not swept")
	assert.Equal(t, model.TaskStatus(model.TaskStatusInProgress), unknownAfter.Status, "unknown is not swept")
}
