package model

import (
	"testing"

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

func TestChannel_BuildImageChannelRevisionInput(t *testing.T) {
	baseURL := "https://example.com/v1"
	cfg := `{"defaults":{"generation":"sync","edit":"sync"}}`
	ch := Channel{Id: 42, BaseURL: &baseURL, ImageExecutionConfig: &cfg}

	input := ch.BuildImageChannelRevisionInput("apitype:0")

	assert.Equal(t, 42, input.ChannelID)
	assert.Equal(t, "https://example.com/v1", input.Endpoint)
	assert.Equal(t, "", input.Proxy)
	// Credential must be a non-secret reference to the channel, never the key.
	assert.Equal(t, "channel:42", input.CredentialRef)
	assert.Equal(t, "apitype:0", input.AdapterVersion)
	require.NotNil(t, input.Settings)
	assert.Equal(t, cfg, string(input.Settings))
}
