package model

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// imageRevisionBuilderForTest mirrors the controller builder for the model
// package's transactional save path: build a revision when the channel carries
// an image execution config, nil otherwise. The adapter version is supplied
// directly because the service registry lives outside the model package.
func imageRevisionBuilderForTest(version string) ChannelRevisionBuilder {
	return func(ch *Channel) (*ChannelRevisionCreate, error) {
		if len(ch.ImageExecutionConfigBytes()) == 0 {
			return nil, nil
		}
		input, err := ch.BuildImageChannelRevision(version)
		if err != nil {
			return nil, err
		}
		return &input, nil
	}
}

func newImageChannelForTest(t *testing.T, name string) Channel {
	t.Helper()
	cfg := `{"defaults":{"generation":"sync","edit":"sync"}}`
	return Channel{
		Type:                 1, // ChannelTypeOpenAI
		Key:                  "test-key",
		Name:                 name,
		Group:                "default",
		Models:               "gpt-image-1",
		ImageExecutionConfig: &cfg,
	}
}

func countRevisions(t *testing.T, channelID int) int64 {
	t.Helper()
	var count int64
	require.NoError(t, DB.Model(&ChannelRevision{}).Where("channel_id = ?", channelID).Count(&count).Error)
	return count
}

func TestInsertChannelWithImageRevision_CreatesChannelAbilitiesAndRevision(t *testing.T) {
	truncateChannelRevisions(t)
	ch := newImageChannelForTest(t, "img-create")

	require.NoError(t, InsertChannelWithImageRevision(&ch, imageRevisionBuilderForTest("test-v1")))
	require.NotZero(t, ch.Id)

	// Channel, ability, and one revision all persisted.
	var db Channel
	require.NoError(t, DB.First(&db, ch.Id).Error)
	assert.Equal(t, countRevisions(t, ch.Id), int64(1))

	var revs []ChannelRevision
	require.NoError(t, DB.Where("channel_id = ?", ch.Id).Find(&revs).Error)
	require.Len(t, revs, 1)
	rev := revs[0]
	assert.Equal(t, "test-v1", rev.AdapterVersion)
	// Credential is a non-secret channel reference, not the key.
	assert.True(t, strings.HasPrefix(rev.CredentialRef, "channel:"))
	assert.NotContains(t, rev.CredentialRef, "test-key")
	// Endpoint froze the resolved type default (BaseURL nil).
	assert.NotEmpty(t, rev.Endpoint)
	// Settings is the versioned snapshot DTO.
	assert.Contains(t, string(rev.Settings), `"schema_version"`)
}

func TestInsertChannelWithImageRevision_NilBuilderSkipsRevision(t *testing.T) {
	truncateChannelRevisions(t)
	ch := newImageChannelForTest(t, "img-noop")

	require.NoError(t, InsertChannelWithImageRevision(&ch, nil))
	require.NotZero(t, ch.Id)
	assert.Equal(t, countRevisions(t, ch.Id), int64(0))
}

func TestInsertChannelWithImageRevision_BuilderErrorRollsBackSave(t *testing.T) {
	truncateChannelRevisions(t)
	ch := newImageChannelForTest(t, "img-rollback")

	errBuilder := ChannelRevisionBuilder(func(*Channel) (*ChannelRevisionCreate, error) {
		return nil, errors.New("revision boom")
	})
	err := InsertChannelWithImageRevision(&ch, errBuilder)
	require.Error(t, err)

	// Atomicity: the channel write must be rolled back so an image-capable
	// channel is never persisted without its matching revision.
	var count int64
	require.NoError(t, DB.Model(&Channel{}).Where("name = ?", "img-rollback").Count(&count).Error)
	assert.Zero(t, count, "channel must not persist when revision creation fails")
	assert.Equal(t, countRevisions(t, ch.Id), int64(0))
}

func TestUpdateChannelWithImageRevision_CreatesNewRevisionKeepingOld(t *testing.T) {
	truncateChannelRevisions(t)
	ch := newImageChannelForTest(t, "img-update")
	require.NoError(t, InsertChannelWithImageRevision(&ch, imageRevisionBuilderForTest("test-v1")))

	var firstRevs []ChannelRevision
	require.NoError(t, DB.Where("channel_id = ?", ch.Id).Order("revision_number").Find(&firstRevs).Error)
	require.Len(t, firstRevs, 1)
	first := firstRevs[0]

	// Second save: change a mutable field and freeze a new revision.
	ch.Name = "img-update-renamed"
	require.NoError(t, UpdateChannelWithImageRevision(&ch, imageRevisionBuilderForTest("test-v1")))

	var allRevs []ChannelRevision
	require.NoError(t, DB.Where("channel_id = ?", ch.Id).Order("revision_number").Find(&allRevs).Error)
	require.Len(t, allRevs, 2)

	// New revision is allocated after the first.
	assert.Greater(t, allRevs[1].RevisionNumber, first.RevisionNumber)
	// The old revision is immutable: its frozen endpoint/number are unchanged.
	assert.Equal(t, first.RevisionNumber, allRevs[0].RevisionNumber)
	assert.Equal(t, first.Endpoint, allRevs[0].Endpoint)
}

func TestUpdateChannelWithImageRevision_BuilderErrorRollsBackUpdate(t *testing.T) {
	truncateChannelRevisions(t)
	ch := newImageChannelForTest(t, "img-update-rb")
	require.NoError(t, InsertChannelWithImageRevision(&ch, imageRevisionBuilderForTest("test-v1")))
	originalName := ch.Name

	ch.Name = "should-not-persist"
	err := UpdateChannelWithImageRevision(&ch, ChannelRevisionBuilder(func(*Channel) (*ChannelRevisionCreate, error) {
		return nil, errors.New("revision boom")
	}))
	require.Error(t, err)

	// Atomicity: the field update and any revision must be rolled back.
	var db Channel
	require.NoError(t, DB.First(&db, ch.Id).Error)
	assert.Equal(t, originalName, db.Name, "update must roll back when revision creation fails")
	assert.Equal(t, countRevisions(t, ch.Id), int64(1), "no extra revision after rollback")
}
