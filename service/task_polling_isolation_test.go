package service

import (
	"context"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFilterImageTasksForLegacyPolling is the pure invariant: only Wischoicer
// image-platform tasks are carved out; every legacy platform (suno, mj, video
// provider strings, empty) stays in the polling set.
func TestFilterImageTasksForLegacyPolling(t *testing.T) {
	tasks := []*model.Task{
		{TaskID: "kling-1", Platform: constant.TaskPlatform("kling")},
		{TaskID: "suno-1", Platform: constant.TaskPlatformSuno},
		{TaskID: "image-1", Platform: constant.TaskPlatformWischoicerImage},
		{TaskID: "mj-1", Platform: constant.TaskPlatformMidjourney},
		{TaskID: "image-2", Platform: constant.TaskPlatformWischoicerImage},
	}
	legacy, imageCount := filterImageTasksForLegacyPolling(tasks)

	assert.Equal(t, 2, imageCount)
	require.Len(t, legacy, 3)
	got := make([]string, len(legacy))
	for i, t := range legacy {
		got[i] = t.TaskID
	}
	assert.Equal(t, []string{"kling-1", "suno-1", "mj-1"}, got)
}

// TestRunTaskPollingOnceSkipsImageTasks proves the full poll pass counts the
// image-platform task as skipped and never admits it into the platform dispatch
// set. The legacy video task is left for the existing dispatch path (exercised
// by TestUpdateVideoTasksCanSkipPollingSleepPerChannel); this test isolates the
// §7.3 carve-out — that an image task present in the unfinished set is filtered
// before any per-platform work, so it cannot reach DispatchPlatformUpdate's
// default UpdateVideoTasks branch.
func TestRunTaskPollingOnceSkipsImageTasks(t *testing.T) {
	truncate(t)

	// GetAllUnFinishSyncTasks applies Limit(TaskQueryLimit); the default is 0 in
	// tests (production sets it via env), which yields no rows. Set a real bound
	// so the seeded tasks are actually fetched through the poll path.
	prevLimit := constant.TaskQueryLimit
	constant.TaskQueryLimit = 100
	t.Cleanup(func() { constant.TaskQueryLimit = prevLimit })

	const channelID = 210
	seedTaskPollingChannel(t, channelID, true)
	seedPollingTask(t, channelID, "poll_legacy", "up_legacy")
	imageTask := &model.Task{
		TaskID:      "poll_image",
		Platform:    constant.TaskPlatformWischoicerImage,
		UserId:      1,
		ChannelId:   channelID,
		Action:      constant.TaskActionGenerate,
		Status:      model.TaskStatusInProgress,
		Progress:    "30%",
		SubmitTime:  time.Now().Unix(),
		CreatedAt:   time.Now().Unix(),
		UpdatedAt:   time.Now().Unix(),
		PrivateData: model.TaskPrivateData{UpstreamTaskID: "up_image"},
	}
	require.NoError(t, model.DB.Create(imageTask).Error)

	// A non-nil adaptor factory is required so RunTaskPollingOnce does not bail
	// on its startup nil-check; the legacy task then takes the existing dispatch
	// path. The assertion targets the summary (set during fetch + filter, before
	// any dispatch), so the outcome of the legacy dispatch does not affect it.
	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	// No adaptor is wired: if the image task were not filtered, RunTaskPollingOnce
	// would still skip dispatch for it (its platform is not Suno/MJ and has no
	// video upstream), but the summary must record it as skipped. Asserting on
	// the summary (not a wired adaptor) keeps this test focused on the filter,
	// independent of the legacy dispatch machinery.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	summary := RunTaskPollingOnce(ctx, nil)

	assert.Equal(t, 2, summary.UnfinishedTasks, "both legacy and image tasks are unfinished in the DB")
	assert.Equal(t, 1, summary.ImageTasksSkipped, "the image task is counted as skipped")
}

// TestSweepTimedOutTasksSkipsImageTasks proves the legacy timeout sweep fails a
// timed-out legacy task but leaves a timed-out image task untouched, so the
// image_task_execution scheduler + billing ledger own its lifecycle (§7.3).
func TestSweepTimedOutTasksSkipsImageTasks(t *testing.T) {
	truncate(t)

	prevTimeout := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 1
	t.Cleanup(func() { constant.TaskTimeoutMinutes = prevTimeout })

	oldSubmit := time.Now().Unix() - 120 // older than the 1-minute cutoff
	legacyTask := &model.Task{
		TaskID:     "to_legacy",
		Platform:   constant.TaskPlatform("kling"),
		UserId:     1,
		Action:     constant.TaskActionGenerate,
		Status:     model.TaskStatusInProgress,
		Progress:   "30%",
		Quota:      0, // no refund path needed for the isolation assertion
		SubmitTime: oldSubmit,
		CreatedAt:  oldSubmit,
		UpdatedAt:  oldSubmit,
	}
	require.NoError(t, model.DB.Create(legacyTask).Error)
	imageTask := &model.Task{
		TaskID:     "to_image",
		Platform:   constant.TaskPlatformWischoicerImage,
		UserId:     1,
		Action:     constant.TaskActionGenerate,
		Status:     model.TaskStatusInProgress,
		Progress:   "30%",
		Quota:      0,
		SubmitTime: oldSubmit,
		CreatedAt:  oldSubmit,
		UpdatedAt:  oldSubmit,
	}
	require.NoError(t, model.DB.Create(imageTask).Error)

	sweepTimedOutTasks(context.Background())

	var legacyAfter model.Task
	require.NoError(t, model.DB.First(&legacyAfter, legacyTask.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), legacyAfter.Status, "legacy timed-out task is swept")

	var imageAfter model.Task
	require.NoError(t, model.DB.First(&imageAfter, imageTask.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusInProgress), imageAfter.Status, "image task must not be swept by legacy timeout")
}
