package service

import (
	"fmt"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
)

// ImageTaskCreateAllowed reports whether the public single-image task create
// routes are released for use. It is the §14.1 route-level release/liveness
// gate: creation is admitted only when an operator has enabled it AND the
// processor is on, so admitted work can advance (the §14.1 startup gate in
// common/init.go already makes create-on without processor-on fatal).
//
// This is a RELEASE gate, NOT an authorization boundary. General token
// authentication is performed by TokenAuth before the handler runs, so by the
// time this gate is reached the caller already holds a valid token; when it
// returns true, any such token may attempt creation. There is no per-principal
// allowlist here. The only in-flow checks CreateImageTask applies after the gate
// are the creation token's optional TokenModelLimit (model), image-capable
// channel selection (channel capability/routing), and the per-user in-flight cap
// (capacity) — none of them is per-principal authorization. The on-state
// authorization surface is intentionally broad (HO 2026-07-18, WIS-569); see the
// rollout observation in WIS-565.
func ImageTaskCreateAllowed() bool {
	return constant.ImageTaskCreateEnabled && constant.ImageTaskProcessorEnabled
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
