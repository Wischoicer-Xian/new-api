package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const procTestAdapterVersion = "test-image-adapter/v1"

// processorMockAdapter is a configurable ImageTaskAdapter for processor tests.
// The fields are mutable so a test can progress the mock across passes (e.g.
// running on the first poll, completed on the second).
type processorMockAdapter struct {
	submitID  string
	submitErr error
	submitFn  func(context.Context) (string, error)
	poll      ImageAdapterPollOutcome
	pollErr   error
	submits   int
	polls     int
}

func (m *processorMockAdapter) Submit(ctx context.Context, _ *model.ChannelRevision, _ string, _ ImageProviderRequest) (string, error) {
	m.submits++
	if m.submitFn != nil {
		return m.submitFn(ctx)
	}
	return m.submitID, m.submitErr
}

func (m *processorMockAdapter) Poll(context.Context, *model.ChannelRevision, string, string) (ImageAdapterPollOutcome, error) {
	m.polls++
	return m.poll, m.pollErr
}

func (m *processorMockAdapter) Cancel(context.Context, *model.ChannelRevision, string, string) error {
	return ErrCancelUnsupported
}

// useProcessorMock registers the mock under the test adapter version for the
// test's lifetime. Re-registering replaces, so this is isolated to the test.
func useProcessorMock(t *testing.T, mock *processorMockAdapter) {
	t.Helper()
	registerImageAdapter(procTestAdapterVersion, mock)
}

// processorClockEnv overrides the processor clock and returns a mutator the test
// advances between passes. The initial value is far from zero so SLA math works.
func processorClockEnv(t *testing.T) func(newNow int64) {
	t.Helper()
	original := imageTaskProcessorClock
	mu := &clockMutator{now: 2_000_000_000}
	imageTaskProcessorClock = func() int64 { return mu.now }
	t.Cleanup(func() { imageTaskProcessorClock = original })
	return func(newNow int64) { mu.now = newNow }
}

type clockMutator struct{ now int64 }

// seedReservedProcessorExecution creates a real reserved image-task execution
// (valid V1 snapshot, applied reserve ledger) backed by a test-adapter channel,
// and returns the freshly-read execution row. Tests then configure the mock and
// drive RunImageTaskProcessorOnce.
func seedReservedProcessorExecution(t *testing.T, ownerID, tokenID int) *model.ImageTaskExecution {
	t.Helper()
	truncate(t)
	saveRestorePriceMaps(t)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"dall-e-3":0.04}`))

	require.NoError(t, model.DB.Create(&model.User{Id: ownerID, Username: "proc-owner", Quota: 100000, Status: common.UserStatusEnabled}).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		Id: tokenID, UserId: ownerID, Key: "sk-proc", Name: "proc-tok",
		Status: common.TokenStatusEnabled, RemainQuota: 100000, ExpiredTime: -1,
	}).Error)

	const chID = 8401
	require.NoError(t, model.DB.Create(&model.Channel{
		Id: chID, Type: constant.ChannelTypeApiNebula, Status: common.ChannelStatusEnabled,
		Group: "default", Models: "dall-e-3", Key: "sk-channel",
	}).Error)
	cfg := `{"defaults":{"generation":"async_task","edit":"async_task"}}`
	require.NoError(t, model.DB.Create(&model.ChannelRevision{
		ChannelID: chID, RevisionNumber: 1, Endpoint: "https://apinebula.test",
		CredentialRef: "channel:" + itoa(chID), AdapterVersion: procTestAdapterVersion,
		Settings: mustImageRevisionSettings(t, cfg),
	}).Error)

	var rev model.ChannelRevision
	require.NoError(t, model.DB.Where("channel_id = ?", chID).First(&rev).Error)
	prevCap := constant.MaxImageTasksPerUser
	constant.MaxImageTasksPerUser = constant.DefaultMaxImageTasksPerUser
	prevInFlight := constant.MaxImageTasksInFlightPerUser
	constant.MaxImageTasksInFlightPerUser = constant.DefaultMaxImageTasksInFlightPerUser
	t.Cleanup(func() {
		constant.MaxImageTasksPerUser = prevCap
		constant.MaxImageTasksInFlightPerUser = prevInFlight
	})

	price, err := resolveImageTaskPrice("dall-e-3", "default", "default", false)
	require.NoError(t, err)
	outcome, err := model.ReserveImageTask(context.Background(), model.ImageTaskReserveCommand{
		OwnerUserID: ownerID, CreationTokenID: tokenID,
		Operation: model.ImageTaskOperationGeneration, IdempotencyKey: "proc-key", RequestHash: "proc-hash",
		ChannelRevisionID: rev.ID, ExecutionMode: "async_task", AdapterVersion: procTestAdapterVersion,
		Price: price, RequestData: []byte(`{"model":"dall-e-3","prompt":"a red cube"}`), Now: 1_700_000_000,
	})
	require.NoError(t, err)
	require.False(t, outcome.Replayed)

	var exec model.ImageTaskExecution
	require.NoError(t, model.DB.First(&exec, outcome.Execution.ID).Error)
	return &exec
}

func itoa(n int) string {
	return commonIntToString(n)
}

func reloadExec(t *testing.T, id int64) model.ImageTaskExecution {
	t.Helper()
	var exec model.ImageTaskExecution
	require.NoError(t, model.DB.First(&exec, id).Error)
	return exec
}

func TestProcessorSubmitAcceptedMovesToPolling(t *testing.T) {
	exec := seedReservedProcessorExecution(t, 5001, 5002)
	mock := &processorMockAdapter{submitID: "task_proc_001"}
	useProcessorMock(t, mock)
	setNow := processorClockEnv(t)
	setNow(2_000_000_100)

	summary := RunImageTaskProcessorOnce(context.Background())
	assert.Equal(t, 1, summary.Claimed)
	assert.Equal(t, 1, summary.StillInProgress)
	assert.Equal(t, 1, mock.submits)

	got := reloadExec(t, exec.ID)
	assert.Equal(t, model.ImageTaskStatePolling, got.State)
	assert.Equal(t, "task_proc_001", got.ClientSubmissionID)
}

func TestProcessorPersistsSubmitIntentAndPreservesConcurrentCancel(t *testing.T) {
	exec := seedReservedProcessorExecution(t, 5003, 5004)
	mock := &processorMockAdapter{}
	mock.submitFn = func(context.Context) (string, error) {
		current := reloadExec(t, exec.ID)
		assert.Equal(t, model.ImageTaskStateSubmitting, current.State, "submit intent must be durable before the provider call")
		won, _, err := model.RequestImageTaskCancelCAS(exec.PublicTaskID, exec.OwnerUserID, 2_000_000_101)
		require.NoError(t, err)
		require.True(t, won)
		return "task_cancel_race", nil
	}
	useProcessorMock(t, mock)
	setNow := processorClockEnv(t)
	setNow(2_000_000_100)

	RunImageTaskProcessorOnce(context.Background())
	got := reloadExec(t, exec.ID)
	assert.Equal(t, model.ImageTaskStateCancelRequested, got.State)
	assert.Equal(t, "task_cancel_race", got.ClientSubmissionID, "accepted upstream id must survive a concurrent cancel")
	assert.NotZero(t, got.CancelRequestedAt)
}

func TestProcessorProviderStepTimeoutRoutesSubmissionUnknown(t *testing.T) {
	exec := seedReservedProcessorExecution(t, 5005, 5006)
	previousTimeout := imageTaskProcessorStepTimeout
	imageTaskProcessorStepTimeout = 10 * time.Millisecond
	t.Cleanup(func() { imageTaskProcessorStepTimeout = previousTimeout })
	mock := &processorMockAdapter{submitFn: func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", networkSubmitError(ImageProviderStageSubmit, ctx.Err())
	}}
	useProcessorMock(t, mock)
	setNow := processorClockEnv(t)
	setNow(2_000_000_100)

	RunImageTaskProcessorOnce(context.Background())
	got := reloadExec(t, exec.ID)
	assert.Equal(t, model.ImageTaskStateSubmissionUnknown, got.State)
}

func TestProcessorCorruptOperationFailsClosedBeforeSubmit(t *testing.T) {
	exec := seedReservedProcessorExecution(t, 5007, 5008)
	require.NoError(t, model.DB.Model(&model.ImageTaskExecution{}).Where("id = ?", exec.ID).Update("operation", "corrupt").Error)
	mock := &processorMockAdapter{submitID: "must-not-submit"}
	useProcessorMock(t, mock)
	setNow := processorClockEnv(t)
	setNow(2_000_000_100)

	RunImageTaskProcessorOnce(context.Background())
	got := reloadExec(t, exec.ID)
	assert.Equal(t, model.ImageTaskStateManualReview, got.State)
	assert.Zero(t, mock.submits)
}

func TestProcessorSubmit429RetriesThenAccepts(t *testing.T) {
	exec := seedReservedProcessorExecution(t, 5011, 5012)
	mock := &processorMockAdapter{submitErr: &ImageProviderError{Kind: ImageErrSubmissionUnknown, Stage: ImageProviderStageSubmit, Status: 429}}
	useProcessorMock(t, mock)
	setNow := processorClockEnv(t)
	now := int64(2_000_000_100)
	setNow(now)

	// First pass: 429 → stays queued with backoff.
	RunImageTaskProcessorOnce(context.Background())
	got := reloadExec(t, exec.ID)
	assert.Equal(t, model.ImageTaskStateQueued, got.State)
	assert.Equal(t, 1, got.SubmitErrorCount)

	// Second pass after backoff: accept.
	mock.submitErr = nil
	mock.submitID = "task_proc_002"
	setNow(now + 1000)
	RunImageTaskProcessorOnce(context.Background())
	got = reloadExec(t, exec.ID)
	assert.Equal(t, model.ImageTaskStatePolling, got.State)
	assert.Equal(t, "task_proc_002", got.ClientSubmissionID)
}

func TestProcessorSubmitNetworkErrorRoutesSubmissionUnknown(t *testing.T) {
	exec := seedReservedProcessorExecution(t, 5021, 5022)
	mock := &processorMockAdapter{submitErr: &ImageProviderError{Kind: ImageErrSubmissionUnknown, Stage: ImageProviderStageSubmit, Status: 500}}
	useProcessorMock(t, mock)
	setNow := processorClockEnv(t)
	setNow(2_000_000_100)

	RunImageTaskProcessorOnce(context.Background())
	got := reloadExec(t, exec.ID)
	assert.Equal(t, model.ImageTaskStateSubmissionUnknown, got.State, "network/5xx must not re-submit")
}

func TestProcessorPreSubmitSafeFinalizesFailed(t *testing.T) {
	exec := seedReservedProcessorExecution(t, 5031, 5032)
	mock := &processorMockAdapter{submitErr: &ImageProviderError{Kind: ImageErrPreSubmitSafe, Stage: ImageProviderStageSubmit, Status: 400, UpstreamMessage: "bad model"}}
	useProcessorMock(t, mock)
	setNow := processorClockEnv(t)
	setNow(2_000_000_100)

	summary := RunImageTaskProcessorOnce(context.Background())
	assert.Equal(t, 1, summary.Failed)
	got := reloadExec(t, exec.ID)
	assert.Equal(t, model.ImageTaskStateFailed, got.State)
}

func TestProcessorPollRunningBacksOff(t *testing.T) {
	exec := seedReservedProcessorExecution(t, 5041, 5042)
	// Move execution into polling with an upstream id.
	require.NoError(t, model.DB.Model(&model.ImageTaskExecution{}).Where("id = ?", exec.ID).Updates(map[string]any{
		"state": model.ImageTaskStatePolling, "client_submission_id": "task_run_1", "next_run_at": 1,
	}).Error)

	mock := &processorMockAdapter{poll: ImageAdapterPollOutcome{Status: ImagePollRunning}}
	useProcessorMock(t, mock)
	setNow := processorClockEnv(t)
	setNow(2_000_000_100)

	RunImageTaskProcessorOnce(context.Background())
	got := reloadExec(t, exec.ID)
	assert.Equal(t, model.ImageTaskStatePolling, got.State)
	assert.Equal(t, 1, got.PollCount)
	assert.Greater(t, got.NextRunAt, int64(2_000_000_100), "next_run_at must advance past now")
}

func TestProcessorPollFailedFinalizesFailed(t *testing.T) {
	exec := seedReservedProcessorExecution(t, 5051, 5052)
	require.NoError(t, model.DB.Model(&model.ImageTaskExecution{}).Where("id = ?", exec.ID).Updates(map[string]any{
		"state": model.ImageTaskStatePolling, "client_submission_id": "task_fail_1", "next_run_at": 1,
	}).Error)

	mock := &processorMockAdapter{poll: ImageAdapterPollOutcome{Status: ImagePollFailed, Message: "policy"}}
	useProcessorMock(t, mock)
	setNow := processorClockEnv(t)
	setNow(2_000_000_100)

	summary := RunImageTaskProcessorOnce(context.Background())
	assert.Equal(t, 1, summary.Failed)
	got := reloadExec(t, exec.ID)
	assert.Equal(t, model.ImageTaskStateFailed, got.State)
}

func TestProcessorSubmissionUnknownSLARoutesManualReview(t *testing.T) {
	exec := seedReservedProcessorExecution(t, 5061, 5062)
	sla := int64(constant.ImageTaskSubmissionReconcileSLA.Seconds())
	// Entered submission_unknown well before the SLA.
	require.NoError(t, model.DB.Model(&model.ImageTaskExecution{}).Where("id = ?", exec.ID).Updates(map[string]any{
		"state": model.ImageTaskStateSubmissionUnknown, "updated_at": 1_700_000_000, "next_run_at": 1_700_000_000 + sla,
	}).Error)

	mock := &processorMockAdapter{}
	useProcessorMock(t, mock)
	setNow := processorClockEnv(t)

	// At exactly the SLA deadline, the processor routes to manual_review.
	setNow(1_700_000_000 + sla)
	summary := RunImageTaskProcessorOnce(context.Background())
	assert.Equal(t, 1, summary.ManualReview)
	got := reloadExec(t, exec.ID)
	assert.Equal(t, model.ImageTaskStateManualReview, got.State)
}

func TestProcessorCancelDrainFailedFinalizesCancelled(t *testing.T) {
	exec := seedReservedProcessorExecution(t, 5071, 5072)
	require.NoError(t, model.DB.Model(&model.ImageTaskExecution{}).Where("id = ?", exec.ID).Updates(map[string]any{
		"state": model.ImageTaskStateCancelRequested, "client_submission_id": "task_cxl_1",
		"cancel_requested_at": 1_900_000_000, "next_run_at": 1,
	}).Error)

	mock := &processorMockAdapter{poll: ImageAdapterPollOutcome{Status: ImagePollFailed, Message: "cancelled upstream"}}
	useProcessorMock(t, mock)
	setNow := processorClockEnv(t)
	setNow(2_000_000_100)

	summary := RunImageTaskProcessorOnce(context.Background())
	assert.Equal(t, 1, summary.Cancelled)
	got := reloadExec(t, exec.ID)
	assert.Equal(t, model.ImageTaskStateCancelled, got.State)
}

func TestProcessorCancelDrainTimeoutFinalizesCancelled(t *testing.T) {
	exec := seedReservedProcessorExecution(t, 5081, 5082)
	drainSLA := int64(constant.ImageTaskCancelDrainSLA.Seconds())
	require.NoError(t, model.DB.Model(&model.ImageTaskExecution{}).Where("id = ?", exec.ID).Updates(map[string]any{
		"state": model.ImageTaskStateCancelRequested, "client_submission_id": "task_cxl_2",
		"cancel_requested_at": 1_000_000_000, "next_run_at": 1,
	}).Error)

	mock := &processorMockAdapter{poll: ImageAdapterPollOutcome{Status: ImagePollRunning}}
	useProcessorMock(t, mock)
	setNow := processorClockEnv(t)
	// Now is well past cancel_requested_at + drain SLA → finalize cancelled.
	setNow(1_000_000_000 + drainSLA + 1)

	summary := RunImageTaskProcessorOnce(context.Background())
	assert.Equal(t, 1, summary.Cancelled)
	got := reloadExec(t, exec.ID)
	assert.Equal(t, model.ImageTaskStateCancelled, got.State)
	_ = drainSLA
}

func TestProcessorMissingRevisionRoutesManualReview(t *testing.T) {
	exec := seedReservedProcessorExecution(t, 5091, 5092)
	// Delete the frozen revision → processor cannot load it.
	require.NoError(t, model.DB.Where("channel_id = ?", 8401).Delete(&model.ChannelRevision{}).Error)
	useProcessorMock(t, &processorMockAdapter{})
	setNow := processorClockEnv(t)
	setNow(2_000_000_100)

	summary := RunImageTaskProcessorOnce(context.Background())
	assert.Equal(t, 1, summary.ManualReview)
	got := reloadExec(t, exec.ID)
	assert.Equal(t, model.ImageTaskStateManualReview, got.State)
}

func TestProcessorFairnessCapsPerUser(t *testing.T) {
	// Seed two queued executions for the same user; the per-pass in-flight cap
	// (default 3) still allows both in one pass, so this test instead asserts the
	// cap field is honored by lowering it to 1.
	exec1 := seedReservedProcessorExecution(t, 5101, 5102)
	prevCap := constant.MaxImageTasksInFlightPerUser
	constant.MaxImageTasksInFlightPerUser = 1
	t.Cleanup(func() { constant.MaxImageTasksInFlightPerUser = prevCap })

	// Create a second execution for the same owner via a new idempotency key.
	price, err := resolveImageTaskPrice("dall-e-3", "default", "default", false)
	require.NoError(t, err)
	var rev model.ChannelRevision
	require.NoError(t, model.DB.Where("channel_id = ?", 8401).First(&rev).Error)
	outcome, err := model.ReserveImageTask(context.Background(), model.ImageTaskReserveCommand{
		OwnerUserID: 5101, CreationTokenID: 5102,
		Operation: model.ImageTaskOperationGeneration, IdempotencyKey: "proc-key-2", RequestHash: "proc-hash-2",
		ChannelRevisionID: rev.ID, ExecutionMode: "async_task", AdapterVersion: procTestAdapterVersion,
		Price: price, RequestData: []byte(`{"model":"dall-e-3","prompt":"a blue cube"}`), Now: 1_700_000_010,
	})
	require.NoError(t, err)

	mock := &processorMockAdapter{submitID: "task_a"}
	useProcessorMock(t, mock)
	setNow := processorClockEnv(t)
	setNow(2_000_000_100)

	summary := RunImageTaskProcessorOnce(context.Background())
	assert.Equal(t, 1, summary.Claimed, "per-user cap=1 must process only one execution per pass")
	assert.Equal(t, 1, summary.FairnessSkipped)
	// One of the two advanced to polling; the other stayed queued.
	_ = exec1
	_ = outcome
	queued := 0
	polling := 0
	var execs []model.ImageTaskExecution
	require.NoError(t, model.DB.Where("owner_user_id = ?", 5101).Find(&execs).Error)
	for _, e := range execs {
		switch e.State {
		case model.ImageTaskStateQueued:
			queued++
		case model.ImageTaskStatePolling:
			polling++
		}
	}
	assert.Equal(t, 1, polling)
	assert.Equal(t, 1, queued)
}

// commonIntToString avoids importing strconv at the top of the test for a single
// call; it delegates to fmt-style formatting via common helpers if available.
func commonIntToString(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestProcessorBackoffMonotonicAndCapped(t *testing.T) {
	first := imageTaskBackoffSeconds(1)
	second := imageTaskBackoffSeconds(2)
	capped := imageTaskBackoffSeconds(100)
	max := int64(constant.ImageTaskMaxBackoff.Seconds())
	assert.Greater(t, second, first, "backoff grows with count")
	assert.LessOrEqual(t, capped, max, "backoff capped at max")
	assert.Greater(t, capped, int64(0))
}

func TestProcessorResolveCredentialRejectsBadRef(t *testing.T) {
	_, err := ResolveChannelCredential("not-a-channel-ref")
	assert.Error(t, err)
	_, err = ResolveChannelCredential("channel:")
	assert.Error(t, err)
	_, err = ResolveChannelCredential("channel:abc")
	assert.Error(t, err)
}

func TestProcessorEnsureSentinelDistinct(t *testing.T) {
	// ErrCancelUnsupported must be distinguishable from a generic error so the
	// processor's cancel drain branch is exact.
	assert.NotEqual(t, ErrCancelUnsupported, errors.New("x"))
}
