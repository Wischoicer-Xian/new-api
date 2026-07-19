package common

import (
	"fmt"
	"math"
	"strconv"

	"github.com/shopspring/decimal"
)

// MoneyToCents parses a currency money string (e.g. "10.00") into integer
// cents using exact decimal arithmetic. It is the single canonical conversion
// from a channel money string to the minimum currency unit: the order-creation
// path and the notify-verification path share it so both sides derive cents
// from one function and float64 drift cannot open a gap between them.
//
// Input must be a non-negative value with at most two decimal places. Sub-cent
// input (e.g. "10.009") is rejected instead of being silently truncated to
// 1000 cents — a truncation that could wrongly match a 10.00 order. Any amount
// whose cent representation would overflow int64 is also rejected. Malformed
// or out-of-range input returns an error so the caller ACKs fail without
// entering the credit transaction (r9 P2-4).
func MoneyToCents(money string) (int64, error) {
	d, err := decimal.NewFromString(money)
	if err != nil {
		return 0, fmt.Errorf("invalid money format %q: %w", money, err)
	}
	if d.IsNegative() {
		return 0, fmt.Errorf("money must not be negative: %s", money)
	}
	if !d.Equal(d.Truncate(2)) {
		return 0, fmt.Errorf("money must have at most 2 decimal places: %s", money)
	}
	cents := d.Mul(decimal.NewFromInt(100))
	if cents.GreaterThan(decimal.NewFromInt(math.MaxInt64)) {
		return 0, fmt.Errorf("money amount too large: %s", money)
	}
	return cents.IntPart(), nil
}

// FloatMoneyToCents converts a float64 money value (e.g. TopUp.Money stored as
// float64) to integer cents via the same canonical path a channel money string
// uses: format to two decimals, then parse with MoneyToCents. This guarantees
// the order's frozen expected amount and the notify amount share one
// conversion function, so a value like 9.99 (which float64 stores as
// 9.989999...) round-trips through "9.99" to 999 cents on both sides (r9 P2-4).
func FloatMoneyToCents(money float64) (int64, error) {
	return MoneyToCents(strconv.FormatFloat(money, 'f', 2, 64))
}
