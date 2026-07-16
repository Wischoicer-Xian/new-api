package constant

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
