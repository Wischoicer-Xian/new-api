package model

import (
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// planFunding executes the §5.6 funding selection state table WITHOUT changing
// any balances (§5.5 step 7: "在不改余额的前提下完成 funding plan"). It returns
// the chosen funding source, subscription ID (0 for free/wallet), and the
// applied reserve quota. The actual deductions happen later in the
// ApplyBillingStageTx callback.
//
// The caller passes the selected sub back to the callback via the returned
// *UserSubscription pointer (nil for free/wallet); the row is already locked
// in the outer transaction.
func planFunding(
	tx *gorm.DB,
	cmd ImageTaskReserveCommand,
	fence User,
	now int64,
) (source string, subID int, appliedReserve int, selectedSub *UserSubscription, err error) {
	price := cmd.Price

	// §5.2.3: free model skips the funding table entirely.
	if price.FreeModel() {
		return FundingSourceFree, 0, 0, nil, nil
	}

	R := price.FormulaReserveQuota()
	W := R // wallet required
	S := R // subscription required
	if S < 1 {
		S = 1 // §5.6: subscription minimum = max(R, 1)
	}

	pref := common.NormalizeBillingPreference(fence.GetSetting().BillingPreference)

	// Helper: try wallet (check quota, don't deduct).
	walletOK := func() (bool, error) {
		var u User
		if e := tx.Select("quota").First(&u, cmd.OwnerUserID).Error; e != nil {
			return false, fmt.Errorf("%w: wallet lookup: %v", ErrImageTaskBillingData, e)
		}
		return u.Quota > 0 && u.Quota-W >= 0, nil
	}

	// Helper: lock + plan subscriptions (don't deduct).
	lockAndPlanSubs := func() (foundSub *UserSubscription, allAllowOverflow bool, hasActive bool, pErr error) {
		var subs []UserSubscription
		if e := lockForUpdate(tx).
			Where("user_id = ? AND status = ? AND end_time > ?", cmd.OwnerUserID, "active", now).
			Order("end_time asc, id asc").
			Find(&subs).Error; e != nil {
			return nil, false, false, fmt.Errorf("%w: subscription lookup: %v", ErrImageTaskBillingData, e)
		}
		if len(subs) == 0 {
			return nil, false, false, nil
		}
		allAllow := true
		for i := range subs {
			s := &subs[i]
			plan, e := getSubscriptionPlanByIdTx(tx, s.PlanId)
			if e != nil {
				return nil, false, true, fmt.Errorf("%w: plan lookup: %v", ErrImageTaskBillingData, e)
			}
			if e := maybeResetUserSubscriptionWithPlanTx(tx, s, plan, now); e != nil {
				return nil, false, true, fmt.Errorf("%w: period reset: %v", ErrImageTaskBillingData, e)
			}
			if !s.AllowWalletOverflow {
				allAllow = false
			}
			// First covering sub wins.
			if s.AmountTotal <= 0 || s.AmountTotal-s.AmountUsed >= int64(S) {
				return s, allAllow, true, nil
			}
		}
		return nil, allAllow, true, nil
	}

	switch pref {
	case "wallet_only":
		ok, e := walletOK()
		if e != nil {
			return "", 0, 0, nil, e
		}
		if !ok {
			return "", 0, 0, nil, ErrImageTaskWalletInsufficient
		}
		return FundingSourceWallet, 0, W, nil, nil

	case "subscription_only":
		sub, _, hasActive, e := lockAndPlanSubs()
		if e != nil {
			return "", 0, 0, nil, e
		}
		if !hasActive {
			return "", 0, 0, nil, ErrImageTaskNoActiveSubscription
		}
		if sub == nil {
			return "", 0, 0, nil, ErrImageTaskSubscriptionInsufficient
		}
		return FundingSourceSubscription, sub.Id, S, sub, nil

	case "wallet_first":
		ok, e := walletOK()
		if e != nil {
			return "", 0, 0, nil, e
		}
		if ok {
			return FundingSourceWallet, 0, W, nil, nil
		}
		// Wallet insufficient → try subscription.
		sub, _, hasActive, e := lockAndPlanSubs()
		if e != nil {
			return "", 0, 0, nil, e
		}
		if !hasActive {
			return "", 0, 0, nil, ErrImageTaskNoActiveSubscription
		}
		if sub == nil {
			return "", 0, 0, nil, ErrImageTaskSubscriptionInsufficient
		}
		return FundingSourceSubscription, sub.Id, S, sub, nil

	default: // subscription_first
		sub, allAllow, hasActive, e := lockAndPlanSubs()
		if e != nil {
			return "", 0, 0, nil, e
		}
		if sub != nil {
			return FundingSourceSubscription, sub.Id, S, sub, nil
		}
		// No covering sub.
		if !hasActive {
			// No active sub → wallet fallback.
			break
		}
		// Has active but none covered → check overflow.
		if !allAllow {
			// Any strict → no wallet fallback.
			return "", 0, 0, nil, ErrImageTaskSubscriptionInsufficient
		}
		// All allow → wallet fallback.
	}
	// Wallet fallback (reached from subscription_first no-active or allow-overflow).
	ok, e := walletOK()
	if e != nil {
		return "", 0, 0, nil, e
	}
	if !ok {
		return "", 0, 0, nil, ErrImageTaskWalletInsufficient
	}
	return FundingSourceWallet, 0, W, nil, nil
}

// deductSubscriptionInAggregateTx updates the selected subscription row inside
// ApplyBillingStageTx. Called only when source == subscription.
func deductSubscriptionInAggregateTx(tx *gorm.DB, sub *UserSubscription, amount int) error {
	if amount <= 0 {
		return nil
	}
	sub.AmountUsed += int64(amount)
	if sub.AmountTotal > 0 && sub.AmountUsed > sub.AmountTotal {
		return errors.New("subscription deduction exceeds total")
	}
	return tx.Save(sub).Error
}
