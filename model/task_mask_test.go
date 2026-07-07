package model

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/QuantumNous/new-api/constant"
	commonRelay "github.com/QuantumNous/new-api/relay/common"
)

// TestInitTaskMasksPropertiesForHiddenToken is the contract for desensitization
// point 5 (InitTask writes tasks.properties): a Hidden system token's
// user-visible task Properties (OriginModelName / UpstreamModelName) are stored
// as the alias, while relayInfo's real fields stay intact — the async billing
// path reads PrivateData.BillingContext.OriginModelName, not these properties.
// A non-hidden token keeps the real model in Properties.
func TestInitTaskMasksPropertiesForHiddenToken(t *testing.T) {
	truncateTables(t)
	require.NoError(t, DB.Create(&Token{Id: 3001, UserId: 1, Key: "sk-hidden", Name: "知言云策系统账号", Hidden: true}).Error)
	require.NoError(t, DB.Create(&Token{Id: 3002, UserId: 1, Key: "sk-normal", Name: "normal", Hidden: false}).Error)

	relayInfo := func(tokenId int) *commonRelay.RelayInfo {
		info := &commonRelay.RelayInfo{
			TokenId:         tokenId,
			UserId:          1,
			OriginModelName: "claude-opus-4",
		}
		// UpstreamModelName is promoted from the embedded *ChannelMeta, and
		// InitTask only populates Properties when ChannelMeta != nil.
		info.ChannelMeta = &commonRelay.ChannelMeta{UpstreamModelName: "claude-opus-4-up"}
		return info
	}

	t.Run("hidden token properties masked, relayInfo real fields untouched", func(t *testing.T) {
		info := relayInfo(3001)
		task := InitTask(constant.TaskPlatformSuno, info)

		require.Equal(t, MaskedSystemModelName, task.Properties.OriginModelName)
		require.Equal(t, MaskedSystemModelName, task.Properties.UpstreamModelName)
		// Red line: the source fields used by billing/audit are not mutated.
		require.Equal(t, "claude-opus-4", info.OriginModelName)
		require.Equal(t, "claude-opus-4-up", info.UpstreamModelName)
	})

	t.Run("non-hidden token keeps real model in properties", func(t *testing.T) {
		info := relayInfo(3002)
		task := InitTask(constant.TaskPlatformSuno, info)

		require.Equal(t, "claude-opus-4", task.Properties.OriginModelName)
		require.Equal(t, "claude-opus-4-up", task.Properties.UpstreamModelName)
		require.Equal(t, "claude-opus-4", info.OriginModelName)
	})
}
