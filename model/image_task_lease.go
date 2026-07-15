package model

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// claimableImageTaskStates are the non-terminal states eligible for a worker
// to claim and advance. Terminal states (completed/failed/cancelled) and
// manual_review are excluded: manual_review waits for an operator and must
// not be picked up by automatic processing.
var claimableImageTaskStates = []string{
	string(ImageTaskStateQueued),
	string(ImageTaskStateSubmitting),
	string(ImageTaskStateSubmissionUnknown),
	string(ImageTaskStatePolling),
	string(ImageTaskStateCancelRequested),
}

type ImageTaskLeaseClaim struct {
	ExecutionID int64
	Owner       string
	Now         int64
	LeaseUntil  int64
}

// TryClaimImageTaskExecution leases one execution for processing if it is due
// (next_run_at <= now) and not currently held by an unexpired lease. The whole
// decision is a single conditional UPDATE, so two workers reading the same
// candidate cannot both claim it: only the UPDATE that matches acquires the
// lease. lease_generation is bumped on every claim, giving finalization a
// fencing token — a worker whose lease expired and was taken over will present
// a stale generation and must be rejected when it tries to write results.
//
// Returns the claimed row when won; callers must re-read by ID to confirm
// their own lease_generation before persisting any state.
func TryClaimImageTaskExecution(claim ImageTaskLeaseClaim) (won bool, exec *ImageTaskExecution, err error) {
	if claim.Owner == "" {
		return false, nil, fmt.Errorf("claim image task execution: empty lease owner")
	}
	if claim.LeaseUntil <= claim.Now {
		return false, nil, fmt.Errorf("claim image task execution: lease_until must be after now")
	}
	err = DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&ImageTaskExecution{}).
			Where("id = ? AND next_run_at <= ? AND (lease_until = 0 OR lease_until < ?) AND state IN ?", claim.ExecutionID, claim.Now, claim.Now, claimableImageTaskStates).
			Updates(map[string]any{
				"lease_owner":      claim.Owner,
				"lease_until":      claim.LeaseUntil,
				"lease_generation": gorm.Expr("lease_generation + 1"),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		exec = &ImageTaskExecution{}
		if err := tx.Where("id = ? AND lease_owner = ?", claim.ExecutionID, claim.Owner).First(exec).Error; err != nil {
			return err
		}
		won = true
		return nil
	})
	if err != nil {
		return false, nil, err
	}
	return won, exec, nil
}

type ImageTaskLeaseRenewal struct {
	ExecutionID        int64
	Owner              string
	ExpectedGeneration int
	Now                int64
	LeaseUntil         int64
}

// RenewImageTaskExecutionLease extends the lease only if this owner still
// holds it at the expected generation. A mismatch means another worker took
// over after this lease expired; the caller must stop mutating the row.
// A lease at exactly now is still active because claim uses lease_until < now.
func RenewImageTaskExecutionLease(renewal ImageTaskLeaseRenewal) (won bool, err error) {
	if renewal.Owner == "" {
		return false, fmt.Errorf("renew image task execution lease: empty lease owner")
	}
	if renewal.LeaseUntil <= renewal.Now {
		return false, fmt.Errorf("renew image task execution lease: lease_until must be after now")
	}
	result := DB.Model(&ImageTaskExecution{}).
		Where("id = ? AND lease_owner = ? AND lease_generation = ? AND lease_until >= ? AND state IN ?", renewal.ExecutionID, renewal.Owner, renewal.ExpectedGeneration, renewal.Now, claimableImageTaskStates).
		Update("lease_until", renewal.LeaseUntil)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// IsClaimableImageTaskState reports whether a state is eligible to be claimed
// by a worker, exposed for validation in callers that filter candidates.
func IsClaimableImageTaskState(s ImageTaskExecutionState) bool {
	return common.StringsContains(claimableImageTaskStates, string(s))
}
