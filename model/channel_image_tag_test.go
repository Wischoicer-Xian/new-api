package model

import (
	"errors"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func ptrString(s string) *string { return &s }

func createTaggedChannel(t *testing.T, name, tag string, imageCapable bool) Channel {
	t.Helper()
	cfg := `{"defaults":{"generation":"sync","edit":"sync"}}`
	ch := Channel{
		Type:   1, // ChannelTypeOpenAI
		Key:    "k-" + name,
		Name:   name,
		Tag:    ptrString(tag),
		Group:  "default",
		Models: "m",
	}
	if imageCapable {
		ch.ImageExecutionConfig = &cfg
	}
	require.NoError(t, DB.Create(&ch).Error)
	require.NoError(t, ch.AddAbilities(nil))
	return ch
}

func TestEditChannelByTagWithImageRevision_ImageCapableGetsRevision(t *testing.T) {
	truncateChannelRevisions(t)
	tag := "tag-img"
	img := createTaggedChannel(t, "tag-img-ch", tag, true)
	nonImg := createTaggedChannel(t, "tag-nonimg-ch", tag, false)

	newParam := `{"quality":"hd"}`
	err := EditChannelByTagWithImageRevision(tag, nil, nil, nil, nil, nil, nil, &newParam, nil, imageRevisionBuilderForTest("v1"))
	require.NoError(t, err)

	// The image-capable channel got a new revision freezing the new value; the
	// non-image channel did not.
	assert.Equal(t, countRevisions(t, img.Id), int64(1))
	assert.Equal(t, countRevisions(t, nonImg.Id), int64(0))

	var revs []ChannelRevision
	require.NoError(t, DB.Where("channel_id = ?", img.Id).Find(&revs).Error)
	require.Len(t, revs, 1)
	var snap imageChannelRevisionSnapshot
	require.NoError(t, common.Unmarshal(revs[0].Settings, &snap))
	assert.Equal(t, newParam, snap.ParamOverride)

	// The live channel field was also updated.
	var db Channel
	require.NoError(t, DB.First(&db, img.Id).Error)
	require.NotNil(t, db.ParamOverride)
	assert.Equal(t, newParam, *db.ParamOverride)
}

func TestEditChannelByTagWithImageRevision_NoFrozenChangeSkipsRevisions(t *testing.T) {
	truncateChannelRevisions(t)
	tag := "tag-prio"
	img := createTaggedChannel(t, "tag-prio-ch", tag, true)

	// Changing only priority (not a frozen field) must not create revisions.
	prio := int64(5)
	err := EditChannelByTagWithImageRevision(tag, nil, nil, nil, nil, &prio, nil, nil, nil, imageRevisionBuilderForTest("v1"))
	require.NoError(t, err)
	assert.Equal(t, countRevisions(t, img.Id), int64(0))
}

func TestEditChannelByTagWithImageRevision_BuilderErrorRollsBackFieldUpdate(t *testing.T) {
	truncateChannelRevisions(t)
	tag := "tag-rb"
	img := createTaggedChannel(t, "tag-rb-ch", tag, true)

	// A builder that fails on the image-capable channel must roll the bulk field
	// update back so the frozen-field change never persists without a revision.
	newParam := `{"quality":"hd"}`
	errBuilder := ChannelRevisionBuilder(func(*Channel) (*ChannelRevisionCreate, error) {
		return nil, errors.New("revision boom")
	})
	err := EditChannelByTagWithImageRevision(tag, nil, nil, nil, nil, nil, nil, &newParam, nil, errBuilder)
	require.Error(t, err)

	// Field update rolled back: paramOverride is not the new value.
	var db Channel
	require.NoError(t, DB.First(&db, img.Id).Error)
	assert.Nil(t, db.ParamOverride, "field update must roll back when revision creation fails")
	assert.Equal(t, countRevisions(t, img.Id), int64(0))
}

func countAbilities(t *testing.T, channelID int) int64 {
	t.Helper()
	var count int64
	require.NoError(t, DB.Model(&Ability{}).Where("channel_id = ?", channelID).Count(&count).Error)
	return count
}

// TestEditChannelByTagWithImageRevision_AbilityWriteFailureRollsBack is the
// fault-injection regression for P1-1 atomicity: a GORM callback forces Ability
// creates to fail mid-transaction. The whole tag edit (channel field update +
// ability re-create + revision creation) must roll back so the channel field,
// the ability set and the revision count all stay at the pre-edit state. This
// replaces the prior coverage that only exercised builder errors.
func TestEditChannelByTagWithImageRevision_AbilityWriteFailureRollsBack(t *testing.T) {
	truncateChannelRevisions(t)
	tag := "tag-fault"
	img := createTaggedChannel(t, "tag-fault-ch", tag, true)
	originalModels := img.Models
	originalAbilityCount := countAbilities(t, img.Id)

	cbName := "image-test-fail-ability-create"
	require.NoError(t, DB.Callback().Create().Before("gorm:create").Register(cbName, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "abilities" {
			_ = tx.AddError(errors.New("forced ability write failure"))
		}
	}))
	t.Cleanup(func() { DB.Callback().Create().Remove(cbName) })

	newModels := "new-model-x"
	err := EditChannelByTagWithImageRevision(tag, nil, nil, &newModels, nil, nil, nil, nil, nil, imageRevisionBuilderForTest("v1"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced ability write failure")

	// All-or-nothing: channel field, ability set, and revisions all unchanged.
	var dbCh Channel
	require.NoError(t, DB.First(&dbCh, img.Id).Error)
	assert.Equal(t, originalModels, dbCh.Models, "channel field must roll back on ability write failure")
	assert.Equal(t, originalAbilityCount, countAbilities(t, img.Id), "ability set must be unchanged after rollback")
	assert.Equal(t, countRevisions(t, img.Id), int64(0), "no revision must persist on rollback")
}
