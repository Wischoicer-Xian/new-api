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

// MaxImageTasksPerUser caps how many non-terminal image tasks one user may hold
// at once (§6.1: over the cap returns 429 + Retry-After). The default is a
// conservative placeholder; the product owner owns the final throttle value.
var MaxImageTasksPerUser int

// temporary variable for sora patch, will be removed in future
var TaskPricePatches []string

// TrustedRedirectDomains is a list of trusted domains for redirect URL validation.
// Domains support subdomain matching (e.g., "example.com" matches "sub.example.com").
var TrustedRedirectDomains []string
