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

// imageChannelRevisionSnapshot is the immutable snapshot of the channel
// configuration an image task freezes against at revision creation time, so a
// later channel edit/disable/delete cannot change how an in-flight task shapes
// its provider call. It captures every field the runtime request path consumes
// (middleware/distributor.go + relay/image_handler.go): the image execution
// config, the structured provider settings, the type-specific other settings,
// param/header overrides, the OpenAI organization, and the model mapping.
//
// The channel's secret Key is NEVER stored here. Per §7.2 the credential is a
// reference (ChannelRevision.CredentialRef = "channel:<id>") resolved to the
// channel's current key at runtime, so a key rotation takes effect immediately
// without re-freezing every revision. The overrides are frozen as provider
// settings because they shape the request immutably; if a future product
// decision wants override-embedded secrets to also rotate, the snapshot shape
// (schema_version) is the bump point. The snapshot is processor-internal: it
// lives in ChannelRevision.Settings, is never returned by the preview/read
// endpoints, and never enters logs or SSE.
type imageChannelRevisionSnapshot struct {
	SchemaVersion      int    `json:"schema_version"`
	ExecutionConfig    string `json:"execution_config,omitempty"`
	ProviderSettings   string `json:"provider_settings,omitempty"`   // Channel.Setting (proxy/extras)
	OtherSettings      string `json:"other_settings,omitempty"`      // Channel.OtherSettings (type-specific)
	ParamOverride      string `json:"param_override,omitempty"`      // request param overrides
	HeaderOverride     string `json:"header_override,omitempty"`     // request header overrides
	OpenAIOrganization string `json:"openai_organization,omitempty"` // OpenAI org
	ModelMapping       string `json:"model_mapping,omitempty"`       // model redirect mapping
	StatusCodeMapping  string `json:"status_code_mapping,omitempty"` // error-class / retry mapping
}

// ImageExecutionConfigFromRevision returns the execution configuration frozen
// in a revision. New task selection must use this value, never the mutable
// channel cache, so mode and provider settings share one immutable version.
func ImageExecutionConfigFromRevision(revision *ChannelRevision) ([]byte, error) {
	if revision == nil || len(revision.Settings) == 0 {
		return nil, fmt.Errorf("image channel revision settings are required")
	}
	var snapshot imageChannelRevisionSnapshot
	if err := common.Unmarshal(revision.Settings, &snapshot); err != nil {
		return nil, fmt.Errorf("decode image channel revision settings: %w", err)
	}
	if snapshot.SchemaVersion != imageRevisionSnapshotSchemaVersion {
		return nil, fmt.Errorf("unsupported image channel revision schema version %d", snapshot.SchemaVersion)
	}
	config := strings.TrimSpace(snapshot.ExecutionConfig)
	if config == "" {
		return nil, fmt.Errorf("image channel revision execution config is required")
	}
	return []byte(config), nil
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

// trimmedStringPtr returns the trimmed value of a *string field, or "".
func trimmedStringPtr(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// nonEmptyJSONString returns the trimmed value of a *string JSON field, or ""
// when it is empty or an insignificant "{}" so the snapshot omits no-op config.
func nonEmptyJSONString(p *string) string {
	if p == nil {
		return ""
	}
	trimmed := strings.TrimSpace(*p)
	if trimmed == "" || trimmed == "{}" {
		return ""
	}
	return trimmed
}

// BuildImageChannelRevision constructs the immutable revision snapshot for an
// image-capable channel. Endpoint is the resolved (custom or type-default)
// URL; Proxy is the channel's configured proxy; Settings is a versioned
// snapshot carrying every runtime-consumed provider-settings field (the secret
// key is never included — only the channel reference is, in CredentialRef,
// resolved to the live key at runtime per §7.2). adapterVersion is the adapter
// implementation version supplied by the caller (service layer), not an
// API-type alias.
func (channel *Channel) BuildImageChannelRevision(adapterVersion string) (ChannelRevisionCreate, error) {
	snapshot := imageChannelRevisionSnapshot{
		SchemaVersion:      imageRevisionSnapshotSchemaVersion,
		ExecutionConfig:    string(channel.ImageExecutionConfigBytes()),
		ProviderSettings:   nonEmptyJSONString(channel.Setting),
		OtherSettings:      strings.TrimSpace(channel.OtherSettings),
		ParamOverride:      nonEmptyJSONString(channel.ParamOverride),
		HeaderOverride:     nonEmptyJSONString(channel.HeaderOverride),
		OpenAIOrganization: trimmedStringPtr(channel.OpenAIOrganization),
		ModelMapping:       nonEmptyJSONString(channel.ModelMapping),
		StatusCodeMapping:  nonEmptyJSONString(channel.StatusCodeMapping),
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
//
// nullColumns lists column names the request explicitly set to nil (e.g.
// image_execution_config on a clear). GORM's struct Updates skips nil pointer
// fields, so without this the old value would silently persist; each listed
// column is written to SQL NULL in the SAME transaction, keeping the
// channel + ability + revision write atomic across SQLite/MySQL/PostgreSQL.
func UpdateChannelWithImageRevision(channel *Channel, nullColumns []string, buildRevision ChannelRevisionBuilder) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		channel.recalcMultiKeySize()
		if err := tx.Model(channel).Updates(channel).Error; err != nil {
			return err
		}
		// Explicit NULL writes for fields the request cleared. Struct Updates
		// skips nil pointers; this closes the gap so a null patch actually
		// persists NULL instead of leaving the prior value.
		for _, column := range nullColumns {
			if err := tx.Model(channel).Update(column, nil).Error; err != nil {
				return err
			}
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

// BatchInsertChannelsWithImageRevision inserts every channel in the batch, its
// abilities, and (via buildRevision) its immutable revision inside a SINGLE
// transaction. This preserves the all-or-nothing semantics of BatchInsertChannels
// for image-capable batches: if any channel's ability write or revision build or
// persist fails, the whole batch rolls back so a retry never produces a partial
// or duplicated batch. buildRevision runs per channel after its Create so the
// channel id is backfilled before the snapshot/credential reference is built.
func BatchInsertChannelsWithImageRevision(channels []Channel, buildRevision ChannelRevisionBuilder) error {
	if len(channels) == 0 {
		return nil
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		for i := range channels {
			channel := &channels[i]
			if err := tx.Create(channel).Error; err != nil {
				return err
			}
			if err := channel.AddAbilities(tx); err != nil {
				return err
			}
			if buildRevision == nil {
				continue
			}
			revision, err := buildRevision(channel)
			if err != nil {
				return err
			}
			if revision == nil {
				continue
			}
			if err := validateRevisionSettings(revision.Settings); err != nil {
				return err
			}
			if _, err := createChannelRevisionInTx(tx, *revision); err != nil {
				return err
			}
		}
		return nil
	})
}
