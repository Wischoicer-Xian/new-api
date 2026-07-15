package model

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"gorm.io/gorm"
)

// imageRevisionSnapshotSchemaVersion is the versioned shape of the JSON frozen
// into ChannelRevision.Settings. Bump it when the snapshot DTO changes
// incompatibly so a frozen image task can detect the schema it was created
// against and the processor can migrate or reject older snapshots deliberately.
const imageRevisionSnapshotSchemaVersion = 1

// imageChannelRevisionSnapshot is the immutable, non-secret snapshot of the
// channel configuration an image task freezes against at revision creation
// time. The secret key is never stored (the credential lives as a reference in
// ChannelRevision.CredentialRef and is resolved to the channel's current key at
// runtime); only the connection parameters and execution-relevant provider
// settings needed to reproduce the task's provider call are captured.
type imageChannelRevisionSnapshot struct {
	SchemaVersion    int             `json:"schema_version"`
	ExecutionConfig  json.RawMessage `json:"execution_config,omitempty"`
	ProviderSettings json.RawMessage `json:"provider_settings,omitempty"`
}

// recalcMultiKeySize recomputes MultiKeySize (and trims stale per-key status)
// for multi-key channels. It is shared by Channel.Update and the transactional
// image-revision save path so both apply identical bookkeeping.
func (channel *Channel) recalcMultiKeySize() {
	if !channel.ChannelInfo.IsMultiKey {
		return
	}
	var keyStr string
	if channel.Key != "" {
		keyStr = channel.Key
	} else if existing, err := GetChannelById(channel.Id, true); err == nil {
		// If key is not provided, read the existing key from the database.
		keyStr = existing.Key
	}
	keys := []string{}
	if keyStr != "" {
		trimmed := strings.TrimSpace(keyStr)
		if strings.HasPrefix(trimmed, "[") {
			var arr []json.RawMessage
			if err := common.Unmarshal([]byte(trimmed), &arr); err == nil {
				keys = make([]string, len(arr))
				for i, v := range arr {
					keys[i] = string(v)
				}
			}
		}
		if len(keys) == 0 { // fallback to newline split
			keys = strings.Split(strings.Trim(keyStr, "\n"), "\n")
		}
	}
	channel.ChannelInfo.MultiKeySize = len(keys)
	// Clean up status data that exceeds the new key count to prevent index out of range.
	if channel.ChannelInfo.MultiKeyStatusList != nil {
		for idx := range channel.ChannelInfo.MultiKeyStatusList {
			if idx >= channel.ChannelInfo.MultiKeySize {
				delete(channel.ChannelInfo.MultiKeyStatusList, idx)
			}
		}
	}
}

// resolvedImageEndpoint returns the endpoint an image task connects to,
// resolving the channel type's default base URL when the channel has no custom
// BaseURL (nil or empty). Channel.GetBaseURL only falls back for an empty
// string; this also covers the nil case so a standard channel freezes its real
// endpoint rather than an empty one.
func (channel *Channel) resolvedImageEndpoint() string {
	if channel.BaseURL != nil {
		if url := strings.TrimSpace(*channel.BaseURL); url != "" {
			return url
		}
	}
	return constant.ChannelBaseURLs[channel.Type]
}

// ImageExecutionConfigBytes returns the raw JSON bytes of the channel's image
// task execution configuration, or nil when the channel is not configured for
// image tasks. Whitespace-only values are treated as unset so an admin saving
// an empty field never produces a phantom image-capable channel that would
// enter the candidate pool with nothing to execute.
func (channel *Channel) ImageExecutionConfigBytes() []byte {
	if channel == nil || channel.ImageExecutionConfig == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*channel.ImageExecutionConfig)
	if trimmed == "" {
		return nil
	}
	return []byte(trimmed)
}

// BuildImageChannelRevision constructs the immutable revision snapshot for an
// image-capable channel. Endpoint is the resolved (custom or type-default)
// URL; Proxy is the channel's configured proxy; Settings is a versioned,
// non-secret snapshot DTO carrying the image execution config and the
// adapter's provider settings (the secret key is never included — only the
// channel reference is, in CredentialRef, resolved to the live key at
// runtime). adapterVersion is the adapter implementation version supplied by
// the caller (service layer), not an API-type alias.
func (channel *Channel) BuildImageChannelRevision(adapterVersion string) (ChannelRevisionCreate, error) {
	snapshot := imageChannelRevisionSnapshot{
		SchemaVersion: imageRevisionSnapshotSchemaVersion,
	}
	if configBytes := channel.ImageExecutionConfigBytes(); len(configBytes) > 0 {
		snapshot.ExecutionConfig = append(snapshot.ExecutionConfig, configBytes...)
	}
	if channel.Setting != nil {
		if setting := strings.TrimSpace(*channel.Setting); setting != "" && setting != "{}" {
			snapshot.ProviderSettings = append(snapshot.ProviderSettings, []byte(setting)...)
		}
	}
	settingsBytes, err := common.Marshal(snapshot)
	if err != nil {
		return ChannelRevisionCreate{}, fmt.Errorf("marshal image channel revision snapshot: %w", err)
	}
	return ChannelRevisionCreate{
		ChannelID:      channel.Id,
		Endpoint:       channel.resolvedImageEndpoint(),
		Proxy:          channel.GetSetting().Proxy,
		CredentialRef:  fmt.Sprintf("channel:%d", channel.Id),
		AdapterVersion: adapterVersion,
		Settings:       settingsBytes,
	}, nil
}

// ChannelRevisionBuilder builds the immutable revision for a channel once its
// id is known (after Create for inserts). Returning a nil revision means the
// channel is not image-capable and no revision should be created; returning an
// error aborts the surrounding transaction so the save fails explicitly.
type ChannelRevisionBuilder func(*Channel) (*ChannelRevisionCreate, error)

// InsertChannelWithImageRevision creates the channel, its abilities, and, when
// buildRevision returns a non-nil revision, the immutable revision in a single
// transaction. buildRevision runs after Create so the channel id is known and
// the CredentialRef/snapshot are built against the persisted row. A revision
// build or persist failure aborts the channel creation so the caller surfaces
// an explicit save failure rather than persisting an image-capable channel
// with no matching revision (§7.2 atomicity).
func InsertChannelWithImageRevision(channel *Channel, buildRevision ChannelRevisionBuilder) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(channel).Error; err != nil {
			return err
		}
		if err := channel.AddAbilities(tx); err != nil {
			return err
		}
		if buildRevision == nil {
			return nil
		}
		revision, err := buildRevision(channel)
		if err != nil {
			return err
		}
		if revision == nil {
			return nil
		}
		if err := validateRevisionSettings(revision.Settings); err != nil {
			return err
		}
		_, err = createChannelRevisionInTx(tx, *revision)
		return err
	})
}

// UpdateChannelWithImageRevision updates the channel and its abilities and,
// when buildRevision returns a non-nil revision, freezes the revision in the
// same transaction. A revision build or persist failure rolls the channel
// update back so the save fails explicitly rather than leaving a channel whose
// persisted config has no matching revision (§7.2 atomicity).
func UpdateChannelWithImageRevision(channel *Channel, buildRevision ChannelRevisionBuilder) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		channel.recalcMultiKeySize()
		if err := tx.Model(channel).Updates(channel).Error; err != nil {
			return err
		}
		if err := tx.Model(channel).First(channel, "id = ?", channel.Id).Error; err != nil {
			return err
		}
		if err := channel.UpdateAbilities(tx); err != nil {
			return err
		}
		if buildRevision == nil {
			return nil
		}
		revision, err := buildRevision(channel)
		if err != nil {
			return err
		}
		if revision == nil {
			return nil
		}
		if err := validateRevisionSettings(revision.Settings); err != nil {
			return err
		}
		_, err = createChannelRevisionInTx(tx, *revision)
		return err
	})
}
