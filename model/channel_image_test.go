package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannel_ImageExecutionConfigBytes(t *testing.T) {
	t.Run("nil pointer is unset", func(t *testing.T) {
		var ch Channel
		assert.Nil(t, ch.ImageExecutionConfigBytes())
	})
	t.Run("empty and whitespace only are unset", func(t *testing.T) {
		for _, v := range []string{"", "   ", "\n\t ", "  \n "} {
			v := v
			ch := Channel{ImageExecutionConfig: &v}
			assert.Nil(t, ch.ImageExecutionConfigBytes(), "value %q must be unset", v)
		}
	})
	t.Run("value returned trimmed", func(t *testing.T) {
		s := `  {"defaults":{"generation":"sync"}}  `
		ch := Channel{ImageExecutionConfig: &s}
		got := ch.ImageExecutionConfigBytes()
		require.NotNil(t, got)
		assert.Equal(t, `{"defaults":{"generation":"sync"}}`, string(got))
	})
}

func TestChannel_ResolvedImageEndpoint(t *testing.T) {
	// A standard channel with no custom BaseURL must freeze the type default
	// endpoint, not an empty string (P1-1).
	t.Run("nil BaseURL resolves type default", func(t *testing.T) {
		ch := Channel{Type: constant.ChannelTypeOpenAI}
		assert.Equal(t, constant.ChannelBaseURLs[constant.ChannelTypeOpenAI], ch.resolvedImageEndpoint())
	})
	t.Run("empty BaseURL resolves type default", func(t *testing.T) {
		empty := ""
		ch := Channel{Type: constant.ChannelTypeOpenAI, BaseURL: &empty}
		assert.Equal(t, constant.ChannelBaseURLs[constant.ChannelTypeOpenAI], ch.resolvedImageEndpoint())
	})
	t.Run("custom BaseURL wins", func(t *testing.T) {
		custom := "https://my-proxy.example/v1"
		ch := Channel{Type: constant.ChannelTypeOpenAI, BaseURL: &custom}
		assert.Equal(t, "https://my-proxy.example/v1", ch.resolvedImageEndpoint())
	})
}

func TestChannel_BuildImageChannelRevision(t *testing.T) {
	custom := "https://my-proxy.example/v1"
	cfg := `{"defaults":{"generation":"sync","edit":"sync"}}`
	setting := `{"proxy":"http://corp-proxy:7890","system_prompt":"x"}`
	ch := Channel{
		Id:                   42,
		Type:                 constant.ChannelTypeOpenAI,
		BaseURL:              &custom,
		Setting:              &setting,
		ImageExecutionConfig: &cfg,
	}

	input, err := ch.BuildImageChannelRevision("openai-image-adapter/v1")
	require.NoError(t, err)

	assert.Equal(t, 42, input.ChannelID)
	assert.Equal(t, "https://my-proxy.example/v1", input.Endpoint)
	// Proxy comes from the channel's structured Setting, not a hardcoded empty.
	assert.Equal(t, "http://corp-proxy:7890", input.Proxy)
	// Credential is a non-secret reference to the channel, never the key.
	assert.Equal(t, "channel:42", input.CredentialRef)
	assert.Equal(t, "openai-image-adapter/v1", input.AdapterVersion)

	// Settings is a versioned snapshot DTO carrying execution config + the
	// non-secret provider settings; the secret key is never present.
	require.NotNil(t, input.Settings)
	var snapshot imageChannelRevisionSnapshot
	require.NoError(t, common.Unmarshal(input.Settings, &snapshot))
	assert.Equal(t, imageRevisionSnapshotSchemaVersion, snapshot.SchemaVersion)
	require.NotNil(t, snapshot.ExecutionConfig)
	assert.Equal(t, cfg, string(snapshot.ExecutionConfig))
	require.NotNil(t, snapshot.ProviderSettings)
	var provider map[string]any
	require.NoError(t, common.Unmarshal(snapshot.ProviderSettings, &provider))
	assert.Equal(t, "http://corp-proxy:7890", provider["proxy"])

	t.Run("omits empty provider settings", func(t *testing.T) {
		ch := Channel{Id: 1, Type: constant.ChannelTypeOpenAI, BaseURL: &custom, ImageExecutionConfig: &cfg}
		input, err := ch.BuildImageChannelRevision("openai-image-adapter/v1")
		require.NoError(t, err)
		var snapshot imageChannelRevisionSnapshot
		require.NoError(t, common.Unmarshal(input.Settings, &snapshot))
		assert.Nil(t, snapshot.ProviderSettings)
	})

	t.Run("nil BaseURL freezes type default endpoint", func(t *testing.T) {
		ch := Channel{Id: 1, Type: constant.ChannelTypeOpenAI, ImageExecutionConfig: &cfg}
		input, err := ch.BuildImageChannelRevision("openai-image-adapter/v1")
		require.NoError(t, err)
		assert.Equal(t, constant.ChannelBaseURLs[constant.ChannelTypeOpenAI], input.Endpoint)
	})

	// Guard against the snapshot silently drifting: the marshaled settings must
	// round-trip through common.Unmarshal and carry schema_version.
	t.Run("settings round-trip as valid json", func(t *testing.T) {
		var raw map[string]any
		require.NoError(t, common.Unmarshal(input.Settings, &raw))
		assert.Equal(t, float64(imageRevisionSnapshotSchemaVersion), raw["schema_version"])
	})
}
