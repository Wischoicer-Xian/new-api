package model

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
)

// ImageTaskBillingSnapshotV1 is the §5.3 wire struct stored in the ledger's
// billing_snapshot column. It is constructed once inside the aggregate
// transaction from the frozen price value object + the tx-resolved funding
// rows, validated, and marshaled exactly once. It is never built piecemeal or
// UPDATEd after creation. settle/refund read it as the durable billing truth.
type ImageTaskBillingSnapshotV1 struct {
	SnapshotVersion            int             `json:"snapshot_version"`
	OwnerUserID                int             `json:"owner_user_id"`
	ResolvedGroup              string          `json:"resolved_group"`
	CreationTokenID            int             `json:"creation_token_id"`
	Operation                  string          `json:"operation"`
	OriginModel                string          `json:"origin_model"`
	ChannelRevisionID          int64           `json:"channel_revision_id"`
	PricingFingerprint         string          `json:"pricing_fingerprint"`
	PricingMode                string          `json:"pricing_mode"`
	PricingSource              string          `json:"pricing_source"`
	MatchedModel               string          `json:"matched_model"`
	ModelPrice                 float64         `json:"model_price"`
	ModelRatio                 float64         `json:"model_ratio"`
	GroupRatio                 float64         `json:"group_ratio"`
	QuotaPerUnit               float64         `json:"quota_per_unit"`
	FreeModelPreconsumeEnabled bool            `json:"free_model_preconsume_enabled"`
	FreeModel                  bool            `json:"free_model"`
	FormulaReserveQuota        int             `json:"formula_reserve_quota"`
	AppliedReserveQuota        int             `json:"applied_reserve_quota"`
	FundingSource              string          `json:"funding_source"` // free|wallet|subscription
	SubscriptionID             int             `json:"subscription_id"`
	TokenCacheDigest           string          `json:"token_cache_digest"`
	WischoicerAttribution      json.RawMessage `json:"wischoicer_attribution,omitempty"`
	SubmitRequestID            string          `json:"submit_request_id,omitempty"`
	SubmitUpstreamRequestID    string          `json:"submit_upstream_request_id,omitempty"`
}

// fundingSourceFree/Wallet/Subscription are the §5.3 funding-source values.
const (
	FundingSourceFree         = "free"
	FundingSourceWallet       = "wallet"
	FundingSourceSubscription = "subscription"
)

// Validate checks the §5.3 invariants on a fully-constructed snapshot. It is
// called once before the snapshot is marshaled to the ledger.
func (s *ImageTaskBillingSnapshotV1) Validate() error {
	if s.SnapshotVersion != 1 {
		return fmt.Errorf("billing snapshot: unsupported version %d", s.SnapshotVersion)
	}
	if s.OwnerUserID <= 0 {
		return fmt.Errorf("billing snapshot: owner_user_id required")
	}
	if s.ResolvedGroup == "" {
		return fmt.Errorf("billing snapshot: resolved_group required")
	}
	if s.Operation == "" {
		return fmt.Errorf("billing snapshot: operation required")
	}
	if s.OriginModel == "" {
		return fmt.Errorf("billing snapshot: origin_model required")
	}
	if s.PricingFingerprint == "" {
		return fmt.Errorf("billing snapshot: pricing_fingerprint required")
	}
	if s.FormulaReserveQuota < 0 || s.FormulaReserveQuota > common.MaxQuota {
		return fmt.Errorf("billing snapshot: formula_reserve_quota out of range %d", s.FormulaReserveQuota)
	}
	if s.AppliedReserveQuota < 0 || s.AppliedReserveQuota > common.MaxQuota {
		return fmt.Errorf("billing snapshot: applied_reserve_quota out of range %d", s.AppliedReserveQuota)
	}
	switch s.FundingSource {
	case FundingSourceFree:
		if !s.FreeModel || s.AppliedReserveQuota != 0 {
			return fmt.Errorf("billing snapshot: free requires free_model=true and applied=0")
		}
		if s.SubscriptionID != 0 {
			return fmt.Errorf("billing snapshot: free must have subscription_id=0")
		}
	case FundingSourceWallet:
		if s.FreeModel {
			return fmt.Errorf("billing snapshot: wallet requires free_model=false")
		}
		if s.SubscriptionID != 0 {
			return fmt.Errorf("billing snapshot: wallet must have subscription_id=0")
		}
		if s.AppliedReserveQuota != s.FormulaReserveQuota {
			return fmt.Errorf("billing snapshot: wallet applied must equal formula")
		}
	case FundingSourceSubscription:
		if s.FreeModel {
			return fmt.Errorf("billing snapshot: subscription requires free_model=false")
		}
		if s.SubscriptionID <= 0 {
			return fmt.Errorf("billing snapshot: subscription requires subscription_id>0")
		}
		if s.AppliedReserveQuota < 1 {
			return fmt.Errorf("billing snapshot: subscription applied must be >=1")
		}
	default:
		return fmt.Errorf("billing snapshot: unknown funding_source %q", s.FundingSource)
	}
	return nil
}

// Marshal serializes the snapshot using common.Marshal (§5.3: marshal once).
func (s *ImageTaskBillingSnapshotV1) Marshal() ([]byte, error) {
	return common.Marshal(s)
}

// buildSnapshotFromPriceAndFunding constructs a complete V1 snapshot from the
// frozen price value object and the tx-resolved funding result. This is the
// ONLY way a snapshot is created (§5.3: caller cannot supply snapshot JSON).
func buildSnapshotFromPriceAndFunding(
	price *ImageTaskPriceResolution,
	ownerUserID int,
	creationTokenID int,
	operation string,
	channelRevisionID int64,
	tokenCacheDigest string,
	attribution json.RawMessage,
	requestID, upstreamRequestID string,
	fundingSource string,
	subscriptionID int,
	appliedReserveQuota int,
) (*ImageTaskBillingSnapshotV1, error) {
	mode := "model_price"
	source := "model_price"
	if price.IsRatioMode() {
		mode = "model_ratio"
		source = "model_ratio"
	}
	snap := &ImageTaskBillingSnapshotV1{
		SnapshotVersion:            1,
		OwnerUserID:                ownerUserID,
		ResolvedGroup:              price.ResolvedGroup(),
		CreationTokenID:            creationTokenID,
		Operation:                  operation,
		OriginModel:                price.OriginModel(),
		ChannelRevisionID:          channelRevisionID,
		PricingFingerprint:         price.PricingFingerprint(),
		PricingMode:                mode,
		PricingSource:              source,
		MatchedModel:               price.MatchedModel(),
		ModelPrice:                 price.ModelPrice(),
		ModelRatio:                 price.ModelRatio(),
		GroupRatio:                 price.GroupRatio(),
		QuotaPerUnit:               price.QuotaPerUnit(),
		FreeModelPreconsumeEnabled: price.FreeModelPreconsume(),
		FreeModel:                  price.FreeModel(),
		FormulaReserveQuota:        price.FormulaReserveQuota(),
		AppliedReserveQuota:        appliedReserveQuota,
		FundingSource:              fundingSource,
		SubscriptionID:             subscriptionID,
		TokenCacheDigest:           tokenCacheDigest,
		WischoicerAttribution:      attribution,
		SubmitRequestID:            requestID,
		SubmitUpstreamRequestID:    upstreamRequestID,
	}
	if err := snap.Validate(); err != nil {
		return nil, err
	}
	return snap, nil
}

// parseAttribution trims and validates the attribution raw JSON. Empty/nil is
// allowed (no attribution). Non-empty must be valid JSON (via common).
func parseAttribution(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	// Verify it parses without storing the parsed form.
	var probe any
	if err := common.Unmarshal([]byte(trimmed), &probe); err != nil {
		return nil, fmt.Errorf("billing snapshot: invalid attribution JSON: %w", err)
	}
	return json.RawMessage(trimmed), nil
}
