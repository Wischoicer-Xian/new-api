package constant

var StreamingTimeout int
var DifyDebug bool
var MaxFileDownloadMB int
var StreamScannerMaxBufferMB int
var ForceStreamOption bool
var CountToken bool
var GetMediaToken bool
var GetMediaTokenNotStream bool
var UpdateTask bool
var MaxRequestBodyMB int
var AnonymousRequestBodyLimitKB int
var AzureDefaultAPIVersion string
var NotifyLimitCount int
var NotificationLimitDurationMinute int
var GenerateDefaultToken bool
var ErrorLogEnabled bool
var TaskQueryLimit int
var TaskTimeoutMinutes int

// ImageTaskCreateEnabled is the §14.1 create-allowlist placeholder for the
// public single-image task API. It defaults off: the create routes stay
// fail-closed (no task is created) until P3-I wires the real
// principal/channel/model allowlist. P3-C reads it; P3-I replaces it.
var ImageTaskCreateEnabled bool

// ImageTaskProcessorEnabled is the independent §14.1 gate for the image task
// processor (P3). It defaults off: the processor system task stays dormant and
// the scheduler creates no image_task_processor rows until an operator turns it
// on. This is separate from ImageTaskCreateEnabled so read + processor can drain
// in-flight tasks while create stays closed (§14.1: accept/create => read &&
// processor; in-flight tasks require read && processor to stay on).
var ImageTaskProcessorEnabled bool

// DefaultMaxImageTasksPerUser is the dormant, provisional per-user in-flight
// image task cap (§6.1). The final throttle value is a product-owner decision
// that 夏洛克/Krislliu close before any create route goes live; until then this
// default is provisional and MUST NOT be presented as a closed rate-limit
// decision. common.ParseMaxImageTasksPerUser returns it when the env is unset.
const DefaultMaxImageTasksPerUser = 10

// MaxImageTasksPerUser caps how many non-terminal image tasks one user may hold
// at once (§6.1). It must be positive: common.ParseMaxImageTasksPerUser fails
// startup on 0/negative/non-numeric, so the §6.1/§12.1/§17 mandatory cap can
// never be silently disabled. Initialized from MAX_IMAGE_TASKS_PER_USER.
var MaxImageTasksPerUser int

// temporary variable for sora patch, will be removed in future
var TaskPricePatches []string

// TrustedRedirectDomains is a list of trusted domains for redirect URL validation.
// Domains support subdomain matching (e.g., "example.com" matches "sub.example.com").
var TrustedRedirectDomains []string
