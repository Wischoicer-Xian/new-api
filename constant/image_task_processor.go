package constant

import "time"

// Image task processor policy constants (§7.5). These are compile-time freeze
// points for the submit/poll/result-store budgets, the backoff schedule, and
// the manual-review SLAs. They are deliberately conservative: the processor is
// a bounded due-work pass driven by the existing 15 s system-task scheduler, so
// these values bound one execution's progress per claim, not wall-clock latency.

// ImageTask adapter version labels frozen into ChannelRevision.AdapterVersion
// and ImageTaskExecution.AdapterVersion. The processor dispatches its adapter
// implementation by this label, and a frozen execution detects whether it runs
// against the implementation it was created under (§7.2).
const (
	ImageAdapterVersionOpenAI    = "openai-image-adapter/v1"
	ImageAdapterVersionApiNebula = "apinebula-image-adapter/v1"
)

// Per-stage error budgets (§7.5: submit/poll/result-store maintain independent
// deadline and error counts). Reaching the budget does not switch channels
// (forbidden by §7.5); it routes the execution to manual_review or a terminal
// provider failure so the billing aggregate can settle/refund exactly once.

// ImageTaskSubmitErrorBudget is the maximum submit attempts before a
// submission_unknown execution is routed to manual_review for operator
// reconciliation. Submit errors are submission_unknown (network/5xx/timeout)
// or pre_submit_safe/terminal_provider (which finalize immediately).
const ImageTaskSubmitErrorBudget = 5

// ImageTaskPollErrorBudget is the maximum retryable poll errors (5xx/429)
// before the execution is routed to manual_review. A provider running state
// only increments poll_count and never consumes this budget.
const ImageTaskPollErrorBudget = 10

// ImageTaskResultErrorBudget is the maximum result-store attempts (download/
// validate/persist) before the execution is routed to manual_review. Result-
// store errors retry only persistence, never re-submit or re-poll.
const ImageTaskResultErrorBudget = 5

// Backoff schedule (exponential, capped). next_run_at is set to now + backoff so
// the due-work pass reclaims the execution on a later scheduler tick; the
// processor never sleeps in-loop (§7.5: the 15 s scheduler is the fallback, not
// a task sleep loop).
const (
	ImageTaskInitialBackoff = 15 * time.Second
	ImageTaskMaxBackoff     = 10 * time.Minute
)

// ImageTaskSubmissionReconcileSLA bounds how long an execution may live in
// submission_unknown before it enters manual_review (§7.5). ApiNebula accepts
// no client idempotency key upstream, so a lost submit response cannot be
// reconciled by query; after this window the execution is unrecoverable
// without an operator binding the upstream id by audit.
const ImageTaskSubmissionReconcileSLA = 30 * time.Minute

// ImageTaskCancelDrainSLA bounds the detached cancel drain (§7.5 cancel
// absorption). ApiNebula exposes no cancel endpoint, so a cancel_requested
// execution polls the upstream to a terminal state; if the upstream never
// terminates within this window the execution finalizes as cancelled (refund),
// never hanging on user capacity indefinitely.
const ImageTaskCancelDrainSLA = 30 * time.Minute

// DefaultMaxImageTasksInFlightPerUser is the dormant per-user concurrency cap
// for the processor's bounded due-work pass (§7.5 fairness). It bounds how many
// of one user's executions a single processor pass may lease at once so a hot
// account cannot monopolize the worker. Distinct from MaxImageTasksPerUser
// (the total non-terminal cap); this is the per-pass fairness limit.
const DefaultMaxImageTasksInFlightPerUser = 3

// MaxImageTasksInFlightPerUser is the per-pass per-user concurrency cap. Must
// be positive; ParseMaxImageTasksInFlightPerUser fails startup otherwise.
var MaxImageTasksInFlightPerUser int

// ImageTaskProcessorClaimLease is the lease duration a processor claim holds.
// It must exceed one scheduler tick (15 s) so a claimed execution is not
// re-claimed by a concurrent pass mid-work, and be short enough that a crashed
// worker's execution is reclaimed promptly.
const ImageTaskProcessorClaimLease = 90 * time.Second

// ImageTaskProcessorStepTimeout bounds one provider HTTP or result download
// step below the execution lease. This leaves time for the fenced DB write
// after a timed-out network call and prevents a worker from hanging past its
// lease when RELAY_TIMEOUT is disabled.
const ImageTaskProcessorStepTimeout = 60 * time.Second

// ImageTaskProcessorDuePageSize bounds one due-work pass's candidate fetch. The
// pass claims at most this many candidates per tick, then processes each; the
// 15 s scheduler is the fallback that re-runs the pass.
const ImageTaskProcessorDuePageSize = 50

// ImageTaskResultMaxBytes is the upper bound on a downloaded image result.
// ApiNebula returns b64_json-marked results served from a download_url; the
// processor downloads, validates, and persists up to this size into the durable
// result store (§7.6).
const ImageTaskResultMaxBytes = 25 * 1024 * 1024 // 25 MiB

// ImageTaskProviderResponseMaxBytes bounds provider submit/poll JSON bodies.
// Provider error pages are untrusted too and must not be read without a limit.
const ImageTaskProviderResponseMaxBytes = 1 * 1024 * 1024 // 1 MiB
