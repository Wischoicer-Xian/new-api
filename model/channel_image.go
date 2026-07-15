package model

import (
	"fmt"
	"strings"
)

// ImageExecutionConfigBytes returns the raw JSON bytes of the channel's image
// task execution configuration, or nil when the channel is not configured for
// image tasks. Whitespace-only values are treated as unset so an admin saving
// an empty field never produces a phantom image-capable channel that would
// enter the candidate pool with nothing to execute.
func (ch *Channel) ImageExecutionConfigBytes() []byte {
	if ch == nil || ch.ImageExecutionConfig == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*ch.ImageExecutionConfig)
	if trimmed == "" {
		return nil
	}
	return []byte(trimmed)
}

// BuildImageChannelRevisionInput constructs the immutable revision snapshot for
// an image-capable channel. CredentialRef is a non-secret pointer to the
// channel; per the design, the live key is resolved to its current value at
// runtime by the processor, so a key rotation takes effect immediately without
// re-freezing every revision. The secret key itself never lands in the
// revision. Proxy is left empty until a channel-level proxy source exists; the
// processor resolves the effective proxy at runtime.
func (ch *Channel) BuildImageChannelRevisionInput(adapterVersion string) ChannelRevisionCreate {
	return ChannelRevisionCreate{
		ChannelID:      ch.Id,
		Endpoint:       ch.GetBaseURL(),
		Proxy:          "",
		CredentialRef:  fmt.Sprintf("channel:%d", ch.Id),
		AdapterVersion: adapterVersion,
		Settings:       ch.ImageExecutionConfigBytes(),
	}
}
