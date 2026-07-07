package common

import (
	"testing"

	commonpkg "github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestRelayInfoMaskedModelName(t *testing.T) {
	const real = "claude-sonnet-4-5"

	t.Run("hidden true returns alias without mutating OriginModelName", func(t *testing.T) {
		info := &RelayInfo{TokenHidden: true, OriginModelName: real}
		require.Equal(t, commonpkg.MaskedSystemModelAlias, info.MaskedModelName())
		// Red line: the real field must remain intact for billing/audit.
		require.Equal(t, real, info.OriginModelName)
	})

	t.Run("hidden false returns real model (no false masking)", func(t *testing.T) {
		info := &RelayInfo{TokenHidden: false, OriginModelName: real}
		require.Equal(t, real, info.MaskedModelName())
	})

	t.Run("nil receiver is safe", func(t *testing.T) {
		var info *RelayInfo
		require.Equal(t, "", info.MaskedModelName())
	})
}
