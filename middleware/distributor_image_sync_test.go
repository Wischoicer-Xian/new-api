package middleware

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
)

// WIS-580: the affinity-preferred selection path
// (GetPreferredChannelByAffinity -> channelSupportsRequestPath) must not re-adopt
// an async-only image channel (e.g. ChannelTypeApiNebula) for a synchronous image
// request — that channel has no sync GetAdaptor and would 500. The random
// selection path is filtered at the model layer; this guard covers the affinity
// bypass.
func TestChannelSupportsRequestPath_ExcludesAsyncOnlyImageChannel(t *testing.T) {
	async := &model.Channel{Type: constant.ChannelTypeApiNebula}
	sync := &model.Channel{Type: constant.ChannelTypeOpenAI}

	// Sync image path: async-only channel rejected; sync-capable channel kept.
	assert.False(t, channelSupportsRequestPath(async, "/v1/images/generations", "gpt-image-2"),
		"affinity must not re-adopt async-only ApiNebula for a sync image request")
	assert.False(t, channelSupportsRequestPath(async, "/v1/images/edits", "gpt-image-2"))
	assert.True(t, channelSupportsRequestPath(sync, "/v1/images/generations", "gpt-image-2"))

	// Non-image path: the sync-image exclusion rule does not apply.
	assert.True(t, channelSupportsRequestPath(async, "/v1/chat/completions", "gpt-4o"))

	// nil channel still returns false.
	assert.False(t, channelSupportsRequestPath(nil, "/v1/images/generations", "gpt-image-2"))
}
