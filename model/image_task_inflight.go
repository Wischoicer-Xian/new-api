package model

// CountInFlightImageTasksByOwner counts a user's non-terminal image task
// executions. §6.1 uses this to cap per-user in-flight image tasks: over the
// cap, the create route returns 429 with Retry-After.
//
// Every non-terminal state counts, including manual_review (awaiting an
// operator) and submission_unknown (awaiting reconcile), because each still
// holds user capacity. Terminal states (completed/failed/cancelled) do not.
func CountInFlightImageTasksByOwner(ownerUserID int) (int64, error) {
	var count int64
	err := DB.Model(&ImageTaskExecution{}).
		Where("owner_user_id = ?", ownerUserID).
		Where("state NOT IN ?", terminalImageTaskStateStrings).
		Count(&count).Error
	if err != nil {
		return 0, err
	}
	return count, nil
}
