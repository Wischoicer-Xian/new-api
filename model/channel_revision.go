package model

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// ChannelRevision is an immutable snapshot of a channel's non-secret
// configuration at a point in time. When a task starts, it freezes the
// revision it runs against so later edits, disables or deletions of the
// channel do not change how an in-flight task polls or cancels its provider.
// The credential is stored as a reference resolved to the channel's current
// value at runtime, so a key rotation takes effect immediately without
// re-freezing every revision.
type ChannelRevision struct {
	ID             int64           `json:"id" gorm:"primary_key;AUTO_INCREMENT"`
	ChannelID      int             `json:"channel_id" gorm:"index;uniqueIndex:idx_channel_revision,priority:1"`
	RevisionNumber int             `json:"revision_number" gorm:"uniqueIndex:idx_channel_revision,priority:2"`
	Endpoint       string          `json:"endpoint" gorm:"type:varchar(512)"`
	Proxy          string          `json:"proxy" gorm:"type:varchar(255)"`
	Settings       json.RawMessage `json:"settings" gorm:"type:json"`
	CredentialRef  string          `json:"credential_ref" gorm:"type:varchar(255)"`
	AdapterVersion string          `json:"adapter_version" gorm:"type:varchar(64)"`
	CreatedAt      int64           `json:"created_at" gorm:"index"`
}

// ErrChannelRevisionInUse is returned when a revision cannot be deleted
// because a non-terminal task still references it.
var ErrChannelRevisionInUse = errors.New("channel revision is referenced by a non-terminal task")

var ErrChannelRevisionImmutable = errors.New("channel revision is immutable")

func (*ChannelRevision) BeforeUpdate(*gorm.DB) error {
	return ErrChannelRevisionImmutable
}

type ChannelRevisionCreate struct {
	ChannelID      int
	Endpoint       string
	Proxy          string
	CredentialRef  string
	AdapterVersion string
	Settings       json.RawMessage
}

// CreateChannelRevision appends a new immutable revision for a channel. The
// revision number is per-channel and monotonically increasing, computed
// inside the transaction so concurrent creators serialize on the unique
// (channel_id, revision_number) index.
func CreateChannelRevision(input ChannelRevisionCreate) (*ChannelRevision, error) {
	if len(input.Settings) != 0 {
		var decoded any
		if err := common.Unmarshal(input.Settings, &decoded); err != nil {
			return nil, fmt.Errorf("create channel revision: invalid settings JSON: %w", err)
		}
	}
	var rev ChannelRevision
	err := DB.Transaction(func(tx *gorm.DB) error {
		// The atomic UPDATE is the allocation lock. COALESCE handles channels
		// created before the additive counter column existed without a dialect-
		// specific migration default.
		result := tx.Model(&Channel{}).Where("id = ?", input.ChannelID).
			UpdateColumn("image_revision_number", gorm.Expr("COALESCE(image_revision_number, 0) + 1"))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		var channel Channel
		if err := tx.Select("image_revision_number").First(&channel, input.ChannelID).Error; err != nil {
			return err
		}
		rev = ChannelRevision{
			ChannelID:      input.ChannelID,
			RevisionNumber: channel.ImageRevisionNumber,
			Endpoint:       input.Endpoint,
			Proxy:          input.Proxy,
			Settings:       input.Settings,
			CredentialRef:  input.CredentialRef,
			AdapterVersion: input.AdapterVersion,
		}
		return tx.Create(&rev).Error
	})
	if err != nil {
		return nil, err
	}
	return &rev, nil
}

// CountNonTerminalRefs reports how many image task executions in non-terminal
// states reference a revision. It is the gate for revision deletion: a
// revision may be removed only when nothing in flight still needs the frozen
// config to finish.
func CountNonTerminalRefs(revisionID int64) (int64, error) {
	return countNonTerminalRefs(DB, revisionID)
}

func countNonTerminalRefs(tx *gorm.DB, revisionID int64) (int64, error) {
	var count int64
	err := tx.Model(&ImageTaskExecution{}).
		Where("channel_revision_id = ? AND state NOT IN ?", revisionID, terminalImageTaskStateStrings).
		Count(&count).Error
	return count, err
}

// lockImageChannelRevisionFence obtains the same write lock used by revision
// allocation without changing the counter. Starting revision-reference
// creation and deletion with this atomic UPDATE avoids SQLite read-to-write
// lock upgrades while also serializing the operations on MySQL and PostgreSQL.
func lockImageChannelRevisionFence(tx *gorm.DB, channelID int) error {
	result := tx.Model(&Channel{}).Where("id = ?", channelID).
		UpdateColumn("image_revision_number", gorm.Expr("COALESCE(image_revision_number, 0)"))
	if result.Error != nil {
		return result.Error
	}
	// MySQL may report zero affected rows for a no-op UPDATE, so existence is
	// checked explicitly instead of relying on RowsAffected.
	var channel Channel
	return tx.Select("id").First(&channel, channelID).Error
}

// CanDeleteChannelRevision reports whether a revision is safe to delete. Only
// terminal tasks may reference it; any non-terminal execution blocks removal
// because it still needs the frozen endpoint and credential to poll or cancel.
func CanDeleteChannelRevision(revisionID int64) (bool, error) {
	count, err := CountNonTerminalRefs(revisionID)
	if err != nil {
		return false, err
	}
	return count == 0, nil
}

// DeleteChannelRevision deletes a revision unless a non-terminal task
// references it, returning ErrChannelRevisionInUse otherwise.
func DeleteChannelRevision(revisionID int64) error {
	var candidate ChannelRevision
	if err := DB.Select("id", "channel_id").First(&candidate, revisionID).Error; err != nil {
		return err
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		if err := lockImageChannelRevisionFence(tx, candidate.ChannelID); err != nil {
			return err
		}
		var revision ChannelRevision
		if err := lockForUpdate(tx).Where("channel_id = ?", candidate.ChannelID).First(&revision, revisionID).Error; err != nil {
			return err
		}
		count, err := countNonTerminalRefs(tx, revisionID)
		if err != nil {
			return err
		}
		if count != 0 {
			return ErrChannelRevisionInUse
		}
		return tx.Delete(&revision).Error
	})
}
