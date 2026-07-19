package model

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestImageTaskPriceResolution_FormulaGoldenVectors proves the frozen formula
// (§5.2.1) produces the exact expected reserve quotas for each golden case
// in the RFC §5.2.5 table.
func TestImageTaskPriceResolution_FormulaGoldenVectors(t *testing.T) {
	const qpu = 500000.0

	tests := []struct {
		name        string
		mode        string
		source      string
		modelPrice  float64
		modelRatio  float64
		groupRatio  float64
		freePrec    bool
		wantReserve int
		wantFree    bool
		wantErr     bool
	}{
		{
			name: "fixed truncate (raw 42.5)",
			mode: "model_price", source: "model_price",
			modelPrice: 0.000085, groupRatio: 1, freePrec: true,
			wantReserve: 42, wantFree: false,
		},
		{
			name: "fixed fractional (raw 42.9)",
			mode: "model_price", source: "model_price",
			modelPrice: 0.0000858, groupRatio: 1, freePrec: true,
			wantReserve: 42, wantFree: false,
		},
		{
			name: "ratio truncate (raw 42.5)",
			mode: "model_ratio", source: "model_ratio",
			modelRatio: 0.00017, groupRatio: 1, freePrec: true,
			wantReserve: 42, wantFree: false,
		},
		{
			name: "fixed free by price (modelPrice=0, freePrec=false)",
			mode: "model_price", source: "model_price",
			modelPrice: 0, groupRatio: 1, freePrec: false,
			wantReserve: 0, wantFree: true,
		},
		{
			name: "ratio free by group (group=0, freePrec=false)",
			mode: "model_ratio", source: "model_ratio",
			modelRatio: 1, groupRatio: 0, freePrec: false,
			wantReserve: 0, wantFree: true,
		},
		{
			name: "zero wallet (modelPrice=0, freePrec=true)",
			mode: "model_price", source: "model_price",
			modelPrice: 0, groupRatio: 1, freePrec: true,
			wantReserve: 0, wantFree: false,
		},
		{
			name: "subscription minimum (modelRatio=0, freePrec=true)",
			mode: "model_ratio", source: "model_ratio",
			modelRatio: 0, groupRatio: 1, freePrec: true,
			wantReserve: 0, wantFree: false,
		},
		{
			name: "strict max boundary (overflow)",
			mode: "model_price", source: "model_price",
			modelPrice: float64(2147483647) / qpu, groupRatio: 1, freePrec: true,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := NewImageTaskPriceResolution(
				tt.mode, tt.source, "img-v1", "img-v1", "default",
				tt.modelPrice, tt.modelRatio, tt.groupRatio, qpu,
				tt.freePrec, nil,
			)
			if tt.wantErr {
				require.Error(t, err, "overflow must fail closed")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantReserve, v.FormulaReserveQuota(), "formula reserve quota")
			assert.Equal(t, tt.wantFree, v.FreeModel(), "free model flag")
		})
	}
}

// TestImageTaskPriceResolution_CanonicalFingerprint proves the canonical bytes
// layout (§5.2.2) produces the exact SHA-256 digests specified in the RFC
// fixture table.
func TestImageTaskPriceResolution_CanonicalFingerprint(t *testing.T) {
	const qpu = 500000.0

	tests := []struct {
		name            string
		mode            string
		source          string
		selectedRate    float64
		wantFingerprint string
	}{
		{
			name:            "fixed / model_price / 0.000085",
			mode:            "model_price",
			source:          "model_price",
			selectedRate:    0.000085,
			wantFingerprint: "b14e1e878a7b4b44124b0a0534d39daba96780a097606c1aeafbab35f8f5c6ea",
		},
		{
			name:            "ratio / model_ratio / 0.00017",
			mode:            "model_ratio",
			source:          "model_ratio",
			selectedRate:    0.00017,
			wantFingerprint: "0456adc0d9e602bea78aeb092d316b67924a6f034357aef8b5bd87593d1aefa0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var modelPrice, modelRatio float64
			if tt.mode == "model_price" {
				modelPrice = tt.selectedRate
			} else {
				modelRatio = tt.selectedRate
			}
			v, err := NewImageTaskPriceResolution(
				tt.mode, tt.source, "img-v1", "img-v1", "default",
				modelPrice, modelRatio, 1.0, qpu,
				true, nil, // free-preconsume enabled (matches fixture)
			)
			require.NoError(t, err)
			assert.Equal(t, tt.wantFingerprint, v.PricingFingerprint(), "fingerprint must match RFC fixture exactly")
		})
	}
}

// TestImageTaskPriceResolution_ValidationErrors proves the constructor rejects
// invalid inputs.
func TestImageTaskPriceResolution_ValidationErrors(t *testing.T) {
	const qpu = 500000.0

	tests := []struct {
		name    string
		mode    string
		source  string
		origin  string
		matched string
		group   string
		qpu     float64
		wantErr string
	}{
		{name: "unknown mode", mode: "bogus", source: "model_price", origin: "m", matched: "m", group: "g", qpu: qpu, wantErr: "unknown pricing_mode"},
		{name: "mode/source mismatch", mode: "model_price", source: "model_ratio", origin: "m", matched: "m", group: "g", qpu: qpu, wantErr: "model_price mode"},
		{name: "empty origin", mode: "model_price", source: "model_price", origin: "  ", matched: "m", group: "g", qpu: qpu, wantErr: "origin_model"},
		{name: "empty matched", mode: "model_price", source: "model_price", origin: "m", matched: "", group: "g", qpu: qpu, wantErr: "matched_model"},
		{name: "empty group", mode: "model_price", source: "model_price", origin: "m", matched: "m", group: "", qpu: qpu, wantErr: "resolved_group"},
		{name: "zero qpu", mode: "model_price", source: "model_price", origin: "m", matched: "m", group: "g", qpu: 0, wantErr: "quota_per_unit"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewImageTaskPriceResolution(tt.mode, tt.source, tt.origin, tt.matched, tt.group, 0.001, 0, 1, tt.qpu, true, nil)
			require.Error(t, err)
			assert.Contains(t, strings.ToLower(err.Error()), strings.ToLower(tt.wantErr))
		})
	}
}

// TestImageTaskPriceResolution_CanonicalHex verifies the exact canonical byte
// output for the RFC fixture, ensuring the binary layout matches byte-for-byte.
func TestImageTaskPriceResolution_CanonicalHex(t *testing.T) {
	// Build canonical bytes manually for the fixed fixture and compare hex.
	// RFC fixture for fixed/model_price/0.000085:
	wantHex := "7769732e696d6167652e70726963652e763100010100000006696d672d763100000006696d672d76310000000764656661756c743f164840e1719f80411e8480000000003ff000000000000001"

	v, err := NewImageTaskPriceResolution(
		"model_price", "model_price", "img-v1", "img-v1", "default",
		0.000085, 0, 1, 500000, true, nil,
	)
	require.NoError(t, err)

	// Reconstruct canonical bytes by calling the unexported method via a test
	// helper (the method is in the same package).
	gotHex := hex.EncodeToString(v.canonicalBytesForTest())
	assert.Equal(t, wantHex, gotHex, "canonical bytes must match RFC fixture exactly")
}

// canonicalBytesForTest exposes the canonical byte layout for test assertions.
// It reconstructs the same bytes as computeFingerprint but returns them
// instead of hashing.
func (v *ImageTaskPriceResolution) canonicalBytesForTest() []byte {
	buf := make([]byte, 0, 128)
	buf = append(buf, []byte(imageTaskPriceCanonicalPrefix)...)
	buf = append(buf, 0x00)
	buf = append(buf, byte(v.mode))
	buf = append(buf, byte(v.source))
	buf = appendStringBE(buf, v.originModel)
	buf = appendStringBE(buf, v.matchedModel)
	buf = appendStringBE(buf, v.resolvedGroup)
	buf = appendFloat64BE(buf, v.selectedRate)
	buf = appendFloat64BE(buf, v.quotaPerUnit)
	buf = appendFloat64BE(buf, v.groupRatio)
	if v.freeModelPreconsume {
		buf = append(buf, 0x01)
	} else {
		buf = append(buf, 0x00)
	}
	return buf
}
