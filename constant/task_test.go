package constant

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsImageTaskPlatform(t *testing.T) {
	tests := []struct {
		name string
		p    TaskPlatform
		want bool
	}{
		{"wischoicer image platform", TaskPlatformWischoicerImage, true},
		{"suno legacy", TaskPlatformSuno, false},
		{"midjourney legacy", TaskPlatformMidjourney, false},
		{"video provider kling", TaskPlatform("kling"), false},
		{"empty platform", TaskPlatform(""), false},
		{"unknown platform", TaskPlatform("other"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsImageTaskPlatform(tt.p))
		})
	}
}

// TestLegacyPollingPlatformAllowlist pins the closed, positive allowlist that
// backs the §7.3 SQL filter: Suno + every registered video channel type's
// numeric string are members; Midjourney (own poller), image, and unknown are
// not. If a video adaptor is added in relay.GetTaskAdaptor, its channel type
// must be added to LegacyPollingPlatformValues or this test fails.
func TestLegacyPollingPlatformAllowlist(t *testing.T) {
	members := LegacyPollingPlatformValues()
	assert.Contains(t, members, string(TaskPlatformSuno))
	// every registered video channel type is a member
	for _, ct := range []int{
		ChannelTypeAli, ChannelTypeKling, ChannelTypeJimeng, ChannelTypeVertexAi,
		ChannelTypeVidu, ChannelTypeDoubaoVideo, ChannelTypeVolcEngine,
		ChannelTypeSora, ChannelTypeOpenAI, ChannelTypeGemini, ChannelTypeMiniMax,
	} {
		assert.Contains(t, members, strconv.Itoa(ct), "video channel type %d must be in the allowlist", ct)
	}

	mustInclude := []TaskPlatform{TaskPlatformSuno}
	for _, ct := range []int{ChannelTypeAli, ChannelTypeKling, ChannelTypeMiniMax} {
		mustInclude = append(mustInclude, TaskPlatform(strconv.Itoa(ct)))
	}
	for _, p := range mustInclude {
		assert.True(t, IsLegacyPollingPlatform(p), "%s is a legacy polling platform", p)
	}

	mustExclude := []TaskPlatform{
		TaskPlatformWischoicerImage, // image — own scheduler
		TaskPlatformMidjourney,      // MJ — own poller, intentionally not in legacy allowlist
		TaskPlatform(""),            // empty
		TaskPlatform("kling"),       // name string, not the registered numeric form
		TaskPlatform("999"),         // unknown numeric
	}
	for _, p := range mustExclude {
		assert.False(t, IsLegacyPollingPlatform(p), "%s must NOT be a legacy polling platform", p)
	}
}
