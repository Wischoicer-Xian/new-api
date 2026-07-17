package model

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

// ListImageCapableChannelsForGroupModel enumerates the channels registered for
// (group, model) that are configured for image tasks: enabled status and a
// non-empty image execution config. The adapter capability check (whether the
// channel's API type has opted into the image task subsystem) is intentionally
// NOT applied here — it lives in the service layer to avoid a model→service
// import cycle. Callers must still run service.ResolveImageExecution on each
// candidate before selecting it.
//
// §7.5 forbids the image task create path from auto-switching channels (no
// Distribute retry): this function enumerates the candidate pool once and the
// caller selects a single channel, so it never re-distributes. The result
// order is not part of the contract; the service layer sorts by priority
// before picking, so a cache or DB ordering change cannot affect selection.
//
// A normalized model-name fallback mirrors IsChannelEnabledForGroupModel so an
// ability registered on the canonical spelling still resolves when the request
// carries an alias.
func ListImageCapableChannelsForGroupModel(group, model string) []*Channel {
	if group == "" || model == "" {
		return nil
	}
	if !common.MemoryCacheEnabled {
		return listImageCapableChannelsForGroupModelDB(group, model)
	}

	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	if group2model2channels == nil {
		return nil
	}

	out := collectImageConfiguredChannels(group2model2channels[group][model])
	if len(out) > 0 {
		return out
	}
	normalized := ratio_setting.FormatMatchingModelName(model)
	if normalized == "" || normalized == model {
		return nil
	}
	return collectImageConfiguredChannels(group2model2channels[group][normalized])
}

// collectImageConfiguredChannels maps cached channel ids to channels and keeps
// only those still enabled with a non-empty image execution config. The cache
// only holds enabled channels at sync time, but CacheUpdateChannelStatus can
// flip a channel disabled in place, so the status gate is re-checked here.
// Caller must hold channelSyncLock (read lock).
func collectImageConfiguredChannels(ids []int) []*Channel {
	if len(ids) == 0 {
		return nil
	}
	out := make([]*Channel, 0, len(ids))
	for _, id := range ids {
		ch, ok := channelsIDM[id]
		if !ok || !isImageConfiguredChannel(ch) {
			continue
		}
		out = append(out, ch)
	}
	return out
}

// listImageCapableChannelsForGroupModelDB is the MemoryCacheEnabled=false path:
// it reads the ability table directly (exact model, then normalized fallback),
// loads the channels, and applies the same image-configured gate. Used by tests
// that drive a per-test SQLite DB without the in-memory channel cache.
func listImageCapableChannelsForGroupModelDB(group, model string) []*Channel {
	ids := queryAbilityChannelIDsDB(group, model)
	if len(ids) == 0 {
		return nil
	}
	var channels []*Channel
	if err := DB.Where("id IN ?", ids).Find(&channels).Error; err != nil {
		return nil
	}
	out := make([]*Channel, 0, len(channels))
	for _, ch := range channels {
		if isImageConfiguredChannel(ch) {
			out = append(out, ch)
		}
	}
	return out
}

// queryAbilityChannelIDsDB returns the distinct enabled ability channel ids for
// (group, model), falling back to the normalized model name. Mirrors the cache
// path's fallback so DB-driven tests resolve aliases the same way.
func queryAbilityChannelIDsDB(group, model string) []int {
	ids := pluckAbilityChannelIDsDB(group, model)
	if len(ids) > 0 {
		return ids
	}
	normalized := ratio_setting.FormatMatchingModelName(model)
	if normalized == "" || normalized == model {
		return nil
	}
	return pluckAbilityChannelIDsDB(group, normalized)
}

func pluckAbilityChannelIDsDB(group, model string) []int {
	var ids []int
	err := DB.Model(&Ability{}).
		Where(commonGroupCol+" = ? AND model = ? AND enabled = ?", group, model, true).
		Distinct("channel_id").
		Pluck("channel_id", &ids).Error
	if err != nil {
		return nil
	}
	return ids
}

// isImageConfiguredChannel is the model-layer image-capable gate: the channel
// is enabled and carries a non-empty image execution config. The adapter
// capability check is deferred to the service layer (see file doc).
func isImageConfiguredChannel(ch *Channel) bool {
	if ch == nil || ch.Status != common.ChannelStatusEnabled {
		return false
	}
	return len(ch.ImageExecutionConfigBytes()) > 0
}
