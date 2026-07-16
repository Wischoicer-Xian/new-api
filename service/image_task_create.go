package service

import (
	"fmt"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
)

// ImageTaskCreateAllowed reports whether the public single-image task create
// routes may create tasks. It reads the §14.1 create-allowlist placeholder,
// which defaults off (fail-closed: no task is created) until P3-I wires the
// real principal/channel/model allowlist.
func ImageTaskCreateAllowed() bool {
	return constant.ImageTaskCreateEnabled
}

// imageTaskInFlightRetryAfterSeconds is the Retry-After hint (seconds) returned
// with a 429 when a user is over the in-flight cap. A short, fixed backoff:
// image tasks complete asynchronously and free capacity continuously.
const imageTaskInFlightRetryAfterSeconds = 5

// ImageTaskInFlightStatus reports a user's non-terminal image task count
// against the §6.1 per-user cap. AtCap is true when the cap is configured and
// current meets/exceeds it; the caller then rejects the create with 429 +
// RetryAfter. A cap <= 0 disables the limit (AtCap always false).
type ImageTaskInFlightStatus struct {
	Current    int64
	Cap        int
	AtCap      bool
	RetryAfter int
}

// ImageTaskInFlightStatusOf reads the user's in-flight image task count and
// evaluates it against the configured cap.
func ImageTaskInFlightStatusOf(userID int) (ImageTaskInFlightStatus, error) {
	cap := constant.MaxImageTasksPerUser
	current, err := model.CountInFlightImageTasksByOwner(userID)
	if err != nil {
		return ImageTaskInFlightStatus{}, fmt.Errorf("count in-flight image tasks for user %d: %w", userID, err)
	}
	status := ImageTaskInFlightStatus{Current: current, Cap: cap}
	if cap > 0 && current >= int64(cap) {
		status.AtCap = true
		status.RetryAfter = imageTaskInFlightRetryAfterSeconds
	}
	return status, nil
}
