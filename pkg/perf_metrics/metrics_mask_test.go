package perfmetrics

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

// TestRecordRelaySampleMasksHiddenToken is the contract for desensitization
// point 4 (perf_metrics RecordRelaySample): when RelayInfo.TokenHidden was
// injected from the gin context (mirroring TokenUnlimited's double-hop), the
// aggregated perf sample buckets under the masked alias instead of the real
// model, while info.OriginModelName is left untouched — billing/audit still
// read the real field. A non-hidden token's real model flows through unchanged.
// perf_metrics_setting defaults to Enabled=true; recordRedis is a no-op when
// Redis is not configured, so this only observes the in-memory hotBuckets.
//
// Ported from the parallel desensitization candidate (helper in model/) and
// adapted to this PR's helper location (common.MaskedSystemModelAlias) and
// RelayInfo.MaskedModelName() reader.
func TestRecordRelaySampleMasksHiddenToken(t *testing.T) {
	t.Run("hidden token buckets under alias, OriginModelName untouched", func(t *testing.T) {
		clearHotBuckets(t)
		info := &relaycommon.RelayInfo{
			OriginModelName: "claude-opus-4",
			TokenHidden:     true,
			UsingGroup:      "default",
			StartTime:       time.Now(),
		}
		RecordRelaySample(info, true, 100)

		// Red line: RecordRelaySample only reads the field via MaskedModelName(),
		// it never mutates it.
		require.Equal(t, "claude-opus-4", info.OriginModelName)

		masked, real := bucketRequestCounts("claude-opus-4")
		require.Equal(t, int64(1), masked, "hidden token perf sample must bucket under the alias")
		require.Zero(t, real, "real model name must not leak into perf_metrics")
	})

	t.Run("non-hidden token buckets under real model unchanged", func(t *testing.T) {
		clearHotBuckets(t)
		info := &relaycommon.RelayInfo{
			OriginModelName: "claude-opus-4",
			TokenHidden:     false,
			UsingGroup:      "default",
			StartTime:       time.Now(),
		}
		RecordRelaySample(info, true, 100)

		require.Equal(t, "claude-opus-4", info.OriginModelName)

		masked, real := bucketRequestCounts("claude-opus-4")
		require.Zero(t, masked, "non-hidden token must not be masked in perf_metrics")
		require.Equal(t, int64(1), real, "non-hidden token keeps the real model in perf_metrics")
	})
}

func clearHotBuckets(t *testing.T) {
	t.Helper()
	hotBuckets.Range(func(k, _ any) bool {
		hotBuckets.Delete(k)
		return true
	})
}

// bucketRequestCounts sums requestCount across hot buckets, splitting by
// whether the bucket's model is the masked alias or the supplied real model.
func bucketRequestCounts(realModel string) (maskedCount, realCount int64) {
	hotBuckets.Range(func(k, v any) bool {
		key := k.(bucketKey)
		snap := v.(*atomicBucket).snapshot()
		switch key.model {
		case common.MaskedSystemModelAlias:
			maskedCount += snap.requestCount
		case realModel:
			realCount += snap.requestCount
		}
		return true
	})
	return
}
