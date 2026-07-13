package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseMoneyToCents verifies the epay notify money string is parsed to exact
// integer cents (for transaction-side comparison against the frozen order amount)
// and that malformed or negative input is rejected so EpayNotify ACKs fail
// without entering the credit transaction (r8 P1-1).
func TestParseMoneyToCents(t *testing.T) {
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
	}
	for _, tc := range validCases {
		t.Run(tc.name, func(t *testing.T) {
			cents, err := parseMoneyToCents(tc.money)
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
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseMoneyToCents(tc.money)
			require.Error(t, err)
		})
	}
}
