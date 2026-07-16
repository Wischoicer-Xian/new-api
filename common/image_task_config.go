package common

import (
	"fmt"
	"strconv"

	"github.com/QuantumNous/new-api/constant"
)

// ParseMaxImageTasksPerUser validates the MAX_IMAGE_TASKS_PER_USER env value.
// Empty returns constant.DefaultMaxImageTasksPerUser (a dormant provisional
// value, not a closed product decision). A non-empty value must parse as a
// positive integer: 0, negative, or non-numeric are invalid and fail startup
// (fail-closed). §6.1/§12.1/§17 make the per-user cap a mandatory invariant,
// so an illegal value must never be interpreted as unlimited.
func ParseMaxImageTasksPerUser(raw string) (int, error) {
	if raw == "" {
		return constant.DefaultMaxImageTasksPerUser, nil
	}
	cap, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("MAX_IMAGE_TASKS_PER_USER=%q is not an integer", raw)
	}
	if cap <= 0 {
		return 0, fmt.Errorf("MAX_IMAGE_TASKS_PER_USER must be a positive integer, got %d", cap)
	}
	return cap, nil
}
