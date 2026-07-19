package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMoneyToCents verifies the canonical money string → cents conversion.
// Sub-cent input must be rejected (not truncated), overflow must not wrap, and
// the parse must be exact so the notify amount can be compared transaction-side
// against the order's frozen amount without precision loss (r9 P2-4).
func TestMoneyToCents(t *testing.T) {
	validCases := []struct {
		name  string
		money string
		cents int64
	}{
		{name: "two decimals", money: "10.00", cents: 1000},
		{name: "integer form", money: "10", cents: 1000},
		{name: "fractional", money: "9.99", cents: 999},
		{name: "minimum topup", money: "0.01", cents: 1},
		{name: "large amount", money: "999999.99", cents: 99999999},
		{name: "exponent whole currency", money: "1e2", cents: 10000},
		{name: "exponent fractional currency", money: "1e-1", cents: 10},
	}
	for _, tc := range validCases {
		t.Run(tc.name, func(t *testing.T) {
			cents, err := MoneyToCents(tc.money)
			require.NoError(t, err)
			assert.Equal(t, tc.cents, cents)
		})
	}

	invalidCases := []struct {
		name  string
		money string
	}{
		{name: "empty", money: ""},
		{name: "non-numeric", money: "abc"},
		{name: "negative", money: "-1.00"},
		{name: "letters mixed in", money: "10.0a"},
		{name: "sub-cent truncation", money: "10.009"},
		{name: "sub-cent three decimals", money: "9.999"},
		{name: "exponent sub-cent", money: "1e-3"},
		{name: "overflow beyond int64", money: "99999999999999999999"},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := MoneyToCents(tc.money)
			require.Error(t, err)
		})
	}
}

// TestMoneyToCents_RejectsSubCentNotTruncate is the r9 P2-4 core regression:
// "10.009" must NOT silently truncate to 1000 cents (which could wrongly match
// a 10.00 order). It must return an error so the caller ACKs fail.
func TestMoneyToCents_RejectsSubCentNotTruncate(t *testing.T) {
	cents, err := MoneyToCents("10.009")
	require.Error(t, err)
	assert.Equal(t, int64(0), cents, "sub-cent input must not produce a truncated cents value")
}

// TestMoneyToCents_OverflowNoInt64Wrap confirms a money string whose cent
// representation overflows int64 returns an error instead of wrapping via
// big.Int.Int64() silent truncation (r9 P2-4).
func TestMoneyToCents_OverflowNoInt64Wrap(t *testing.T) {
	cents, err := MoneyToCents("99999999999999999999")
	require.Error(t, err)
	assert.Equal(t, int64(0), cents, "overflow must not wrap into a plausible cents value")
}

// TestFloatMoneyToCents verifies the stored float64 money round-trips through
// the same canonical path the channel string uses. 9.99 stored as float64
// (9.989999...) must yield 999 cents on both the expected and notify side,
// eliminating float drift between order creation and verification (r9 P2-4).
func TestFloatMoneyToCents(t *testing.T) {
	cases := []struct {
		name  string
		money float64
		cents int64
	}{
		{name: "whole", money: 10.0, cents: 1000},
		{name: "fractional float drift", money: 9.99, cents: 999},
		{name: "one cent", money: 0.01, cents: 1},
		{name: "zero", money: 0.0, cents: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cents, err := FloatMoneyToCents(tc.money)
			require.NoError(t, err)
			assert.Equal(t, tc.cents, cents)
		})
	}
}

// TestFloatMoneyToCents_NegativeRejects confirms a corrupt negative stored
// money value fails conversion rather than producing negative cents.
func TestFloatMoneyToCents_NegativeRejects(t *testing.T) {
	_, err := FloatMoneyToCents(-1.0)
	require.Error(t, err)
}
