package model

import (
	"errors"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
