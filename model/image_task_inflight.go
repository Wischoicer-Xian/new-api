package model

import "gorm.io/gorm"

// CountInFlightImageTasksByOwner counts a user's non-terminal image task
// executions. §6.1 uses this to cap per-user in-flight image tasks.
//
// Every non-terminal state counts, including manual_review (awaiting an
// operator) and submission_unknown (awaiting reconcile), because each still
// holds user capacity. Terminal states (completed/failed/cancelled) do not.
//
// tx-aware: pass a transaction handle (e.g. the C3 create transaction, after
// lockForUpdate on the owner fence) so the count is serialized with the
// Task/execution/reserve writes in the same transaction — closing the
// count-then-create TOCTOU. Pass nil to use the package-default DB.
func CountInFlightImageTasksByOwner(tx *gorm.DB, ownerUserID int) (int64, error) {
	if tx == nil {
		tx = DB
	}
	var count int64
	err := tx.Model(&ImageTaskExecution{}).
		Where("owner_user_id = ?", ownerUserID).
		Where("state NOT IN ?", terminalImageTaskStateStrings).
		Count(&count).Error
	if err != nil {
		return 0, err
	}
	return count, nil
}
