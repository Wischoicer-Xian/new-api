package model

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strings"

	"github.com/QuantumNous/new-api/common"
)

// ErrUnsupportedImageTaskPricingFacts is returned when the price resolution
// input contains other_ratios or otherwise violates the frozen facts invariant.
var ErrUnsupportedImageTaskPricingFacts = fmt.Errorf("unsupported image task pricing facts")

// imageTaskPricingMode identifies the pricing formula path.
type imageTaskPricingMode uint8

const (
	pricingModeFixed imageTaskPricingMode = 0x01
	pricingModeRatio imageTaskPricingMode = 0x02
)

// imageTaskPricingSource records which rate field the mode selected.
type imageTaskPricingSource uint8

const (
	pricingSourceModelPrice        imageTaskPricingSource = 0x01
	pricingSourceDefaultModelPrice imageTaskPricingSource = 0x02
	pricingSourceModelRatio        imageTaskPricingSource = 0x03
)

const imageTaskPriceSnapshotVersion = 1
const imageTaskPriceCanonicalPrefix = "wis.image.price.v1"

// ImageTaskPriceResolution is an immutable value object that freezes every
// pricing fact the aggregate needs to compute the reserve quota (RFC §5.2). It
// is constructed once from resolved pricing data and cannot be mutated; the
// aggregate uses getters to read formula/fingerprint/funding decisions. The
// constructor validates all inputs, runs the frozen formula (§5.2.1), and
// computes the pricing fingerprint (§5.2.2).
type ImageTaskPriceResolution struct {
	snapshotVersion     int
	mode                imageTaskPricingMode
	source              imageTaskPricingSource
	originModel         string
	matchedModel        string
	resolvedGroup       string
	modelPrice          float64
	modelRatio          float64
	groupRatio          float64
	quotaPerUnit        float64
	freeModelPreconsume bool
	selectedRate        float64
	freeModel           bool
	formulaReserveQuota int
	pricingFingerprint  string
}

// NewImageTaskPriceResolution constructs and validates a frozen price value
// object. mode must be "model_price" or "model_ratio". source must match mode.
// All strings must be non-empty after trim. Floats must be finite, non-negative.
// quotaPerUnit must be > 0. The formula is computed exactly once (§5.2.1) and
// the fingerprint is derived from canonical bytes (§5.2.2).
func NewImageTaskPriceResolution(
	mode, source, originModel, matchedModel, resolvedGroup string,
	modelPrice, modelRatio, groupRatio, quotaPerUnit float64,
	freeModelPreconsume bool,
) (*ImageTaskPriceResolution, error) {
	v := &ImageTaskPriceResolution{
		snapshotVersion:     imageTaskPriceSnapshotVersion,
		originModel:         strings.TrimSpace(originModel),
		matchedModel:        strings.TrimSpace(matchedModel),
		resolvedGroup:       strings.TrimSpace(resolvedGroup),
		modelPrice:          modelPrice,
		modelRatio:          modelRatio,
		groupRatio:          groupRatio,
		quotaPerUnit:        quotaPerUnit,
		freeModelPreconsume: freeModelPreconsume,
	}

	// Parse mode + source enums and validate consistency.
	if err := v.parseModeSource(mode, source); err != nil {
		return nil, err
	}

	// Validate strings.
	if v.originModel == "" {
		return nil, fmt.Errorf("image task price resolution: origin_model required")
	}
	if v.matchedModel == "" {
		return nil, fmt.Errorf("image task price resolution: matched_model required")
	}
	if v.resolvedGroup == "" {
		return nil, fmt.Errorf("image task price resolution: resolved_group required")
	}

	// Validate floats: finite, non-negative.
	for _, f := range []float64{v.modelPrice, v.modelRatio, v.groupRatio, v.quotaPerUnit} {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("image task price resolution: float must be finite")
		}
		if f < 0 {
			return nil, fmt.Errorf("image task price resolution: float must be non-negative")
		}
	}
	if v.quotaPerUnit <= 0 {
		return nil, fmt.Errorf("image task price resolution: quota_per_unit must be > 0")
	}

	// Compute selected rate from mode.
	switch v.mode {
	case pricingModeFixed:
		v.selectedRate = v.modelPrice
	case pricingModeRatio:
		v.selectedRate = v.modelRatio
	}

	// Compute formula reserve quota (§5.2.1).
	if err := v.computeFormulaReserveQuota(); err != nil {
		return nil, err
	}

	// Compute canonical fingerprint (§5.2.2).
	fp, err := v.computeFingerprint()
	if err != nil {
		return nil, err
	}
	v.pricingFingerprint = fp

	return v, nil
}

func (v *ImageTaskPriceResolution) parseModeSource(mode, source string) error {
	switch mode {
	case "model_price":
		v.mode = pricingModeFixed
	case "model_ratio":
		v.mode = pricingModeRatio
	default:
		return fmt.Errorf("image task price resolution: unknown pricing_mode %q", mode)
	}
	switch source {
	case "model_price":
		v.source = pricingSourceModelPrice
	case "default_model_price":
		v.source = pricingSourceDefaultModelPrice
	case "model_ratio":
		v.source = pricingSourceModelRatio
	default:
		return fmt.Errorf("image task price resolution: unknown pricing_source %q", source)
	}
	// Mode/source consistency.
	if v.mode == pricingModeFixed && v.source == pricingSourceModelRatio {
		return fmt.Errorf("image task price resolution: model_price mode with model_ratio source")
	}
	if v.mode == pricingModeRatio && (v.source == pricingSourceModelPrice || v.source == pricingSourceDefaultModelPrice) {
		return fmt.Errorf("image task price resolution: model_ratio mode with model_price source")
	}
	return nil
}

// computeFormulaReserveQuota runs the frozen formula (§5.2.1) exactly once,
// then applies QuotaFromFloatStrict. Sets freeModel if the formula is zero and
// free-model preconsume is disabled.
func (v *ImageTaskPriceResolution) computeFormulaReserveQuota() error {
	var raw float64
	switch v.mode {
	case pricingModeFixed:
		// modelPrice * quotaPerUnit * groupRatio (left-to-right float64)
		raw = v.modelPrice * v.quotaPerUnit * v.groupRatio
	case pricingModeRatio:
		// modelRatio / 2 * quotaPerUnit * groupRatio (left-to-right float64)
		raw = v.modelRatio / 2 * v.quotaPerUnit * v.groupRatio
	}

	quota, err := common.QuotaFromFloatStrict(raw)
	if err != nil {
		// NaN, overflow, or >= MaxQuota: fail closed. No Task/ledger/funding.
		return fmt.Errorf("image task price resolution: formula clamp: %w", err)
	}
	v.formulaReserveQuota = quota

	// Determine free model (§5.2.3).
	if !v.freeModelPreconsume && quota == 0 {
		v.freeModel = true
	}
	return nil
}

// computeFingerprint builds canonical bytes (§5.2.2) and returns their SHA-256
// hex digest. The layout is fixed for V1 and must never change in-place.
func (v *ImageTaskPriceResolution) computeFingerprint() (string, error) {
	buf := make([]byte, 0, 128)
	// ASCII prefix + 0x00
	buf = append(buf, []byte(imageTaskPriceCanonicalPrefix)...)
	buf = append(buf, 0x00)
	// uint8 pricing_mode
	buf = append(buf, byte(v.mode))
	// uint8 pricing_source
	buf = append(buf, byte(v.source))
	// strings: uint32 length (big-endian) + UTF-8 bytes
	buf = appendStringBE(buf, v.originModel)
	buf = appendStringBE(buf, v.matchedModel)
	buf = appendStringBE(buf, v.resolvedGroup)
	// float64 selected_rate (8 bytes big-endian)
	buf = appendFloat64BE(buf, v.selectedRate)
	// float64 quota_per_unit
	buf = appendFloat64BE(buf, v.quotaPerUnit)
	// float64 group_ratio
	buf = appendFloat64BE(buf, v.groupRatio)
	// bool free_model_preconsume_enabled (single byte)
	if v.freeModelPreconsume {
		buf = append(buf, 0x01)
	} else {
		buf = append(buf, 0x00)
	}

	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}

func appendStringBE(buf []byte, s string) []byte {
	b := []byte(s)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(b)))
	buf = append(buf, lenBuf[:]...)
	buf = append(buf, b...)
	return buf
}

func appendFloat64BE(buf []byte, f float64) []byte {
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], math.Float64bits(f))
	buf = append(buf, tmp[:]...)
	return buf
}

// --- Getters (return values, not pointers; struct is immutable) ---

func (v *ImageTaskPriceResolution) SnapshotVersion() int       { return v.snapshotVersion }
func (v *ImageTaskPriceResolution) OriginModel() string        { return v.originModel }
func (v *ImageTaskPriceResolution) MatchedModel() string       { return v.matchedModel }
func (v *ImageTaskPriceResolution) ResolvedGroup() string      { return v.resolvedGroup }
func (v *ImageTaskPriceResolution) ModelPrice() float64        { return v.modelPrice }
func (v *ImageTaskPriceResolution) ModelRatio() float64        { return v.modelRatio }
func (v *ImageTaskPriceResolution) GroupRatio() float64        { return v.groupRatio }
func (v *ImageTaskPriceResolution) QuotaPerUnit() float64      { return v.quotaPerUnit }
func (v *ImageTaskPriceResolution) FreeModelPreconsume() bool  { return v.freeModelPreconsume }
func (v *ImageTaskPriceResolution) FreeModel() bool            { return v.freeModel }
func (v *ImageTaskPriceResolution) FormulaReserveQuota() int   { return v.formulaReserveQuota }
func (v *ImageTaskPriceResolution) PricingFingerprint() string { return v.pricingFingerprint }
func (v *ImageTaskPriceResolution) IsFixedMode() bool          { return v.mode == pricingModeFixed }
func (v *ImageTaskPriceResolution) IsRatioMode() bool          { return v.mode == pricingModeRatio }
