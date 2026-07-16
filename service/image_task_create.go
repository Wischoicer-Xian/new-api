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

// ImageTaskInFlightStatus is a READ-ONLY snapshot of a user's in-flight image
// task count against the configured cap (§6.1). It is NOT concurrency-safe
// enforcement: ImageTaskInFlightStatusOf holds no transaction and no owner
// fence, so two concurrent requests can both observe the same count (write
// skew). Callers MUST NOT treat this primitive as a gate.
//
// Real per-user enforcement lands in C3, inside one transaction: idempotent
// replay/conflict first, then lockForUpdate on the owner fence, then
// CountInFlightImageTasksByOwner(tx, owner) + Task/execution + reserve ledger,
// all in that tx (SQLite serial-write, MySQL/PostgreSQL row lock).
type ImageTaskInFlightStatus struct {
	Current int64
	Cap     int
}

// ImageTaskInFlightStatusOf reads the user's in-flight image task count and the
// configured cap. Read-only primitive; see the type doc for why it is not
// enforcement.
func ImageTaskInFlightStatusOf(userID int) (ImageTaskInFlightStatus, error) {
	current, err := model.CountInFlightImageTasksByOwner(model.DB, userID)
	if err != nil {
		return ImageTaskInFlightStatus{}, fmt.Errorf("count in-flight image tasks for user %d: %w", userID, err)
	}
	return ImageTaskInFlightStatus{Current: current, Cap: constant.MaxImageTasksPerUser}, nil
}
