package model

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func truncateChannelRevisions(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		DB.Exec("DELETE FROM image_task_executions")
		DB.Exec("DELETE FROM channel_revisions")
		DB.Exec("DELETE FROM abilities")
		DB.Exec("DELETE FROM channels")
	})
}

func insertRevisionChannel(t *testing.T, id int) {
	t.Helper()
	require.NoError(t, DB.Create(&Channel{Id: id, Key: "test-key"}).Error)
}

func channelRevisionCreate(channelID int, endpoint string) ChannelRevisionCreate {
	return ChannelRevisionCreate{
		ChannelID:      channelID,
		Endpoint:       endpoint,
		CredentialRef:  "c",
		AdapterVersion: "v1",
	}
}

func TestCreateChannelRevision_NumbersMonotonically(t *testing.T) {
	truncateChannelRevisions(t)
	insertRevisionChannel(t, 7)
	first := channelRevisionCreate(7, "https://a")
	first.CredentialRef = "cred-ref"
	first.Settings = json.RawMessage(`{}`)
	r1, err := CreateChannelRevision(first)
	require.NoError(t, err)
	assert.Equal(t, 1, r1.RevisionNumber)

	second := channelRevisionCreate(7, "https://b")
	second.CredentialRef = "cred-ref"
	second.Settings = json.RawMessage(`{}`)
	r2, err := CreateChannelRevision(second)
	require.NoError(t, err)
	assert.Equal(t, 2, r2.RevisionNumber, "revision number must increment per channel")
}

func TestCreateChannelRevision_NumbersScopedPerChannel(t *testing.T) {
	truncateChannelRevisions(t)
	insertRevisionChannel(t, 7)
	insertRevisionChannel(t, 9)
	_, err := CreateChannelRevision(channelRevisionCreate(7, "https://a"))
	require.NoError(t, err)
	rev, err := CreateChannelRevision(channelRevisionCreate(9, "https://b"))
	require.NoError(t, err)
	assert.Equal(t, 1, rev.RevisionNumber, "each channel has its own revision sequence")
}

func TestDeleteChannelRevision_BlockedByNonTerminalTask(t *testing.T) {
	truncateChannelRevisions(t)
	insertRevisionChannel(t, 7)
	rev, err := CreateChannelRevision(channelRevisionCreate(7, "https://a"))
	require.NoError(t, err)

	// A polling task references the revision: deletion must be blocked so the
	// task can keep using the frozen endpoint and credential to drain.
	seq := atomic.AddInt64(&execSeq, 1)
	exec := &ImageTaskExecution{
		PublicTaskID:      "imgtask_rev_block_" + string(rune('a'+int(seq%26))),
		TaskDBID:          seq,
		OwnerUserID:       1,
		Operation:         ImageTaskOperationGeneration,
		IdempotencyKey:    "rev-block-key-" + string(rune('a'+int(seq%26))),
		RequestHash:       "h",
		State:             ImageTaskStatePolling,
		ChannelRevisionID: rev.ID,
	}
	require.NoError(t, DB.Create(exec).Error)

	ok, err := CanDeleteChannelRevision(rev.ID)
	require.NoError(t, err)
	assert.False(t, ok, "non-terminal reference must block deletion")

	err = DeleteChannelRevision(rev.ID)
	require.ErrorIs(t, err, ErrChannelRevisionInUse)
	require.NoError(t, DB.Model(&ImageTaskExecution{}).Where("id = ?", exec.ID).
		Update("state", ImageTaskStateManualReview).Error)
	ok, err = CanDeleteChannelRevision(rev.ID)
	require.NoError(t, err)
	assert.False(t, ok, "manual review is non-terminal and must keep its frozen revision")

	// Once the task reaches a terminal state, the revision may be deleted.
	require.NoError(t, DB.Model(&ImageTaskExecution{}).Where("id = ?", exec.ID).
		Update("state", ImageTaskStateCompleted).Error)
	ok, err = CanDeleteChannelRevision(rev.ID)
	require.NoError(t, err)
	require.True(t, ok)
}

func TestDeleteChannelRevision_AllowedWhenNoRefs(t *testing.T) {
	truncateChannelRevisions(t)
	insertRevisionChannel(t, 7)
	rev, err := CreateChannelRevision(channelRevisionCreate(7, "https://a"))
	require.NoError(t, err)

	err = DeleteChannelRevision(rev.ID)
	require.NoError(t, err)

	var n int64
	DB.Model(&ChannelRevision{}).Where("id = ?", rev.ID).Count(&n)
	assert.EqualValues(t, 0, n)
}

func TestCreateChannelRevision_ConcurrentNumbersConverge(t *testing.T) {
	useConcurrentSQLiteDB(t, "channel_revision_concurrency", &Channel{}, &ChannelRevision{}, &ImageTaskExecution{})
	insertRevisionChannel(t, 7)
	const workers = 8
	type outcome struct {
		revision *ChannelRevision
		err      error
	}
	outcomes := make(chan outcome, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			<-start
			revision, err := CreateChannelRevision(channelRevisionCreate(7, "https://example.com"))
			outcomes <- outcome{revision: revision, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)

	seen := make(map[int]bool, workers)
	for result := range outcomes {
		require.NoError(t, result.err)
		require.NotNil(t, result.revision)
		seen[result.revision.RevisionNumber] = true
	}
	for number := 1; number <= workers; number++ {
		assert.True(t, seen[number], "revision number %d must be allocated exactly once", number)
	}
}

func TestChannelRevision_UpdateIsRejected(t *testing.T) {
	truncateChannelRevisions(t)
	insertRevisionChannel(t, 7)
	revision, err := CreateChannelRevision(channelRevisionCreate(7, "https://a"))
	require.NoError(t, err)

	err = DB.Model(revision).Update("endpoint", "https://changed").Error
	require.ErrorIs(t, err, ErrChannelRevisionImmutable)
}

func TestCreateChannelRevision_RejectsInvalidSettingsJSON(t *testing.T) {
	truncateChannelRevisions(t)
	insertRevisionChannel(t, 7)

	input := channelRevisionCreate(7, "https://a")
	input.Settings = json.RawMessage(`{"broken"`)
	_, err := CreateChannelRevision(input)
	require.Error(t, err)
}

func TestDeleteChannelRevision_ConcurrentExecutionCreateCannotOrphanReference(t *testing.T) {
	useConcurrentSQLiteDB(t, "channel_revision_delete_race", &Channel{}, &ChannelRevision{}, &ImageTaskExecution{})
	insertRevisionChannel(t, 7)
	revision, err := CreateChannelRevision(channelRevisionCreate(7, "https://a"))
	require.NoError(t, err)
	exec := newImageTaskExecution(1, ImageTaskOperationGeneration, "revision-delete-race", "hash")
	exec.ChannelRevisionID = revision.ID

	start := make(chan struct{})
	var createErr, deleteErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, _, createErr = CreateOrGetImageTaskExecution(DB, exec)
	}()
	go func() {
		defer wg.Done()
		<-start
		deleteErr = DeleteChannelRevision(revision.ID)
	}()
	close(start)
	wg.Wait()

	if createErr == nil {
		require.ErrorIs(t, deleteErr, ErrChannelRevisionInUse)
		var count int64
		require.NoError(t, DB.Model(&ChannelRevision{}).Where("id = ?", revision.ID).Count(&count).Error)
		assert.EqualValues(t, 1, count)
		return
	}
	require.NoError(t, deleteErr)
	var count int64
	require.NoError(t, DB.Model(&ImageTaskExecution{}).Where("channel_revision_id = ?", revision.ID).Count(&count).Error)
	assert.Zero(t, count)
}

func TestGetLatestChannelRevisionByChannelID_ReturnsHighestNumber(t *testing.T) {
	truncateChannelRevisions(t)
	const ch = 9101
	insertRevisionChannel(t, ch)
	// Insert out of order so ORDER BY revision_number DESC is actually exercised.
	require.NoError(t, DB.Create(&ChannelRevision{ChannelID: ch, RevisionNumber: 1}).Error)
	require.NoError(t, DB.Create(&ChannelRevision{ChannelID: ch, RevisionNumber: 3}).Error)
	require.NoError(t, DB.Create(&ChannelRevision{ChannelID: ch, RevisionNumber: 2}).Error)

	got, err := GetLatestChannelRevisionByChannelID(ch)
	require.NoError(t, err)
	assert.Equal(t, 3, got.RevisionNumber)
	assert.Equal(t, ch, got.ChannelID)
}

func TestGetLatestChannelRevisionByChannelID_IsolatesPerChannel(t *testing.T) {
	truncateChannelRevisions(t)
	const chA = 9102
	const chB = 9103
	insertRevisionChannel(t, chA)
	insertRevisionChannel(t, chB)
	require.NoError(t, DB.Create(&ChannelRevision{ChannelID: chA, RevisionNumber: 5}).Error)
	require.NoError(t, DB.Create(&ChannelRevision{ChannelID: chB, RevisionNumber: 1}).Error)
	require.NoError(t, DB.Create(&ChannelRevision{ChannelID: chB, RevisionNumber: 2}).Error)

	got, err := GetLatestChannelRevisionByChannelID(chB)
	require.NoError(t, err)
	assert.Equal(t, 2, got.RevisionNumber)
}

func TestGetLatestChannelRevisionByChannelID_NoRevisionIsNotFound(t *testing.T) {
	truncateChannelRevisions(t)
	_, err := GetLatestChannelRevisionByChannelID(9199)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}
