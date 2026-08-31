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
		TaskPlatform("kling"),       // pre-b3c4d972 named alias — NOT pollable
		TaskPlatform("jimeng"),      // pre-b3c4d972 named alias — NOT pollable
		TaskPlatform("999"),         // unknown numeric
	}
	for _, p := range mustExclude {
		assert.False(t, IsLegacyPollingPlatform(p), "%s must NOT be a legacy polling platform", p)
	}
}

// TestLegacyTimeoutSuperset pins the two-domain split (§7.3 + historical
// compatibility): the timeout convergence set is the polling set plus the
// attested pre-b3c4d972 named aliases kling/jimeng, so those rows are still
// swept to failure; image, MJ, and unknown stay excluded from both.
func TestLegacyTimeoutSuperset(t *testing.T) {
	timeout := LegacyTimeoutPlatformValues()
	// superset of the polling set
	for _, v := range LegacyPollingPlatformValues() {
		assert.Contains(t, timeout, v)
	}
	// historical aliases are in the timeout set, not the polling set
	for _, alias := range []string{"kling", "jimeng"} {
		assert.Contains(t, timeout, alias, "%s historical alias must be in the timeout set", alias)
		assert.False(t, IsLegacyPollingPlatform(TaskPlatform(alias)), "%s is not pollable", alias)
		assert.True(t, IsLegacyTimeoutPlatform(TaskPlatform(alias)), "%s is timeout-convergable", alias)
	}

	// image / MJ / unknown stay excluded from the timeout set too
	for _, p := range []TaskPlatform{TaskPlatformWischoicerImage, TaskPlatformMidjourney, TaskPlatform("999"), TaskPlatform("")} {
		assert.False(t, IsLegacyTimeoutPlatform(p), "%s must NOT be timeout-convergable", p)
	}
}

func TestNormalizeTaskAction(t *testing.T) {
	tests := map[string]string{
		"generate":            TaskActionImageToVideo,
		"textGenerate":        TaskActionTextToVideo,
		"firstTailGenerate":   TaskActionFirstTailToVideo,
		"referenceGenerate":   TaskActionReferenceToVideo,
		"remixGenerate":       TaskActionRemix,
		TaskActionTextToVideo: TaskActionTextToVideo,
		"MUSIC":               "MUSIC",
		"custom_action":       "custom_action",
		"":                    "",
	}

	for input, expected := range tests {
		t.Run(input, func(t *testing.T) {
			assert.Equal(t, expected, NormalizeTaskAction(input))
		})
	}
}
