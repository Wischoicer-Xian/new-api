package model

import (
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChannelRevision_MarshalExcludesSensitiveFields is the P1-2 structural
// no-leak guard: the persistence entity's frozen fields carry json:"-", so a
// GENERIC marshal (encoding/json or common.Marshal) of a ChannelRevision can
// never surface the snapshot, credential reference, endpoint or proxy — even
// when the snapshot holds a secret. Any future external output must build a
// dedicated, field-curated DTO; the entity itself is not a response shape.
func TestChannelRevision_MarshalExcludesSensitiveFields(t *testing.T) {
	secret := "SECRET-NO-LEAK"
	rev := ChannelRevision{
		ID:             1,
		ChannelID:      7,
		RevisionNumber: 1,
		Endpoint:       "https://hidden.example/v1",
		Proxy:          "http://hidden-proxy:7890",
		Settings:       json.RawMessage(`{"header_override":"` + secret + `"}`),
		CredentialRef:  "channel:7",
		AdapterVersion: "openai-image-adapter/v1",
	}

	out, err := common.Marshal(rev)
	require.NoError(t, err)
	body := string(out)

	for _, forbidden := range []string{
		secret,
		"endpoint",
		"proxy",
		"settings",
		"credential_ref",
		"adapter_version",
		"https://hidden.example",
		"hidden-proxy",
		"header_override",
	} {
		assert.NotContains(t, body, forbidden, "entity marshal must not expose %q", forbidden)
	}
	// Non-sensitive metadata is still present (the entity is not totally opaque).
	assert.Contains(t, body, `"channel_id":7`)
}
