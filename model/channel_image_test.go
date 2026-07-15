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
	paramOverride := `{"quality":"hd"}`
	headerOverride := `{"X-Tenant":"t1"}`
	org := "org-123"
	modelMapping := `{"gpt-image-1":"dall-e-3"}`
	statusCodeMapping := `{"429":"500"}`
	ch := Channel{
		Id:                   42,
		Type:                 constant.ChannelTypeOpenAI,
		BaseURL:              &custom,
		Setting:              &setting,
		ImageExecutionConfig: &cfg,
		ParamOverride:        &paramOverride,
		HeaderOverride:       &headerOverride,
		OpenAIOrganization:   &org,
		ModelMapping:         &modelMapping,
		StatusCodeMapping:    &statusCodeMapping,
		OtherSettings:        `{"azure_api_version":"2024-02-15"}`,
		Key:                  "secret-key-value",
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

	// Settings is a versioned snapshot carrying every runtime-consumed provider
	// field. The secret Key must never appear in the snapshot.
	require.NotNil(t, input.Settings)
	assert.NotContains(t, string(input.Settings), "secret-key-value")
	var snapshot imageChannelRevisionSnapshot
	require.NoError(t, common.Unmarshal(input.Settings, &snapshot))
	assert.Equal(t, imageRevisionSnapshotSchemaVersion, snapshot.SchemaVersion)
	assert.Equal(t, cfg, snapshot.ExecutionConfig)
	assert.Equal(t, setting, snapshot.ProviderSettings)
	assert.Equal(t, paramOverride, snapshot.ParamOverride)
	assert.Equal(t, headerOverride, snapshot.HeaderOverride)
	assert.Equal(t, org, snapshot.OpenAIOrganization)
	assert.Equal(t, modelMapping, snapshot.ModelMapping)
	assert.Equal(t, statusCodeMapping, snapshot.StatusCodeMapping)
	assert.Equal(t, `{"azure_api_version":"2024-02-15"}`, snapshot.OtherSettings)

	t.Run("omits empty provider settings", func(t *testing.T) {
		ch := Channel{Id: 1, Type: constant.ChannelTypeOpenAI, BaseURL: &custom, ImageExecutionConfig: &cfg}
		input, err := ch.BuildImageChannelRevision("openai-image-adapter/v1")
		require.NoError(t, err)
		var snapshot imageChannelRevisionSnapshot
		require.NoError(t, common.Unmarshal(input.Settings, &snapshot))
		assert.Empty(t, snapshot.ProviderSettings)
		assert.Empty(t, snapshot.ParamOverride)
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

func TestImageRevision_FreezesOverrideSecretAtRestParity(t *testing.T) {
	// At-rest parity (P1-1 scheme A): a secret placed in the live channel's
	// HeaderOverride is frozen verbatim into the revision snapshot, exactly as
	// the live channel stores it (plaintext). This proves the snapshot is parity
	// with the live channel, not a downgrade. The frozen secret lives only in
	// ChannelRevision.Settings (DB, processor-internal); it is never returned by
	// any API — see TestImageCapabilityPreview_ResponseExcludesSnapshotFields.
	secret := "AKIA-SECRET-DO-NOT-LEAK"
	header := `{"Authorization":"Bearer ` + secret + `"}`
	cfg := `{"defaults":{"generation":"sync"}}`
	ch := Channel{
		Id:                   7,
		Type:                 constant.ChannelTypeOpenAI,
		ImageExecutionConfig: &cfg,
		HeaderOverride:       &header,
	}
	input, err := ch.BuildImageChannelRevision("openai-image-adapter/v1")
	require.NoError(t, err)
	assert.Contains(t, string(input.Settings), secret)
}
