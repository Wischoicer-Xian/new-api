package constant

import "strconv"

type TaskPlatform string

const (
	TaskPlatformSuno       TaskPlatform = "suno"
	TaskPlatformMidjourney              = "mj"
	// TaskPlatformWischoicerImage is the platform value carried by Wischoicer
	// single-image tasks (§6.1). The legacy Suno/video poller and the legacy
	// timeout sweep must never process them: image tasks are driven by the
	// image_task_execution scheduler with their own submit/poll/cancel/refund
	// path (§7.3). Defined before any image task is created so the legacy paths
	// exclude it from day one rather than treating it as an unknown video
	// platform that falls into the default UpdateVideoTasks branch.
	TaskPlatformWischoicerImage = "wis_image"
)

// IsImageTaskPlatform reports whether a platform belongs to the Wischoicer
// single-image task family that the legacy poller and timeout sweep must skip.
func IsImageTaskPlatform(p TaskPlatform) bool {
	return p == TaskPlatformWischoicerImage
}

// LegacyPollingPlatformValues is the closed, positive allowlist of task
// platforms the legacy Suno/video poller, timeout sweep, and the
// async_task_poll existence gate may touch (§7.3). It is Suno plus the
// numeric-string platform of every channel type with a registered video
// adaptor. Midjourney has its own poller and is intentionally absent; image
// tasks (wis_image) and any unknown platform are not members, so the legacy DB
// queries exclude them before ORDER/LIMIT — a fail-closed boundary, not a
// post-limit denylist that image tasks could starve.
//
// Keep in sync with the video adaptors registered in relay.GetTaskAdaptor: when
// a new video channel type is registered there, add it here too, or its tasks
// will be fail-closed out of the legacy poller.
func LegacyPollingPlatformValues() []string {
	return []string{
		string(TaskPlatformSuno),
		strconv.Itoa(ChannelTypeAli),
		strconv.Itoa(ChannelTypeKling),
		strconv.Itoa(ChannelTypeJimeng),
		strconv.Itoa(ChannelTypeVertexAi),
		strconv.Itoa(ChannelTypeVidu),
		strconv.Itoa(ChannelTypeDoubaoVideo),
		strconv.Itoa(ChannelTypeVolcEngine),
		strconv.Itoa(ChannelTypeSora),
		strconv.Itoa(ChannelTypeOpenAI),
		strconv.Itoa(ChannelTypeGemini),
		strconv.Itoa(ChannelTypeMiniMax),
	}
}

// IsLegacyPollingPlatform reports whether a platform is in the legacy polling
// allowlist. It backs the in-memory secondary guard; the SQL platform-IN
// filter on the legacy queries is the primary isolation.
func IsLegacyPollingPlatform(p TaskPlatform) bool {
	for _, v := range LegacyPollingPlatformValues() {
		if string(p) == v {
			return true
		}
	}
	return false
}

const (
	SunoActionMusic  = "MUSIC"
	SunoActionLyrics = "LYRICS"

	TaskActionGenerate          = "generate"
	TaskActionTextGenerate      = "textGenerate"
	TaskActionFirstTailGenerate = "firstTailGenerate"
	TaskActionReferenceGenerate = "referenceGenerate"
	TaskActionRemix             = "remixGenerate"
)

var SunoModel2Action = map[string]string{
	"suno_music":  SunoActionMusic,
	"suno_lyrics": SunoActionLyrics,
}
