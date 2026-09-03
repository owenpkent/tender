// Package money represents ledger amounts as exact integer minor units.
//
// There is no floating point in this package and none anywhere else in the
// money path. USDC has six decimals, so the smallest representable unit is
// 0.000001 and an int64 holds a little over nine trillion dollars of it, which
// is more headroom than a payment rail needs.
package money

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Decimals is the number of fractional digits in one unit of the currency.
// The ledger is single-currency for now; a second currency means carrying the
// scale alongside the amount rather than assuming this constant.
const Decimals = 6

var scale = int64(math.Pow10(Decimals))

// Amount is a signed quantity of minor units. Positive is a credit to the
// account it is attached to, negative is a debit.
type Amount int64

// ErrMalformed is returned by Parse for input that is not a decimal amount.
var ErrMalformed = errors.New("money: malformed amount")

// Parse reads a decimal string such as "12.50" into minor units. It rejects
// more precision than the currency has rather than rounding it away, because
// silently dropping a fraction of a cent is how ledgers stop balancing.
func Parse(s string) (Amount, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("%w: empty", ErrMalformed)
	}

	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}

	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" && !hasFrac {
		return 0, fmt.Errorf("%w: %q", ErrMalformed, s)
	}
	if len(frac) > Decimals {
		return 0, fmt.Errorf("%w: %q has more than %d decimal places", ErrMalformed, s, Decimals)
	}

	units, err := parseDigits(whole)
	if err != nil {
		return 0, err
	}
	fracUnits, err := parseDigits(frac + strings.Repeat("0", Decimals-len(frac)))
	if err != nil {
		return 0, err
	}

	if units > (math.MaxInt64-fracUnits)/scale {
		return 0, fmt.Errorf("%w: %q overflows", ErrMalformed, s)
	}
	total := units*scale + fracUnits
	if neg {
		total = -total
	}
	return Amount(total), nil
}

func parseDigits(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%w: %q is not a number", ErrMalformed, s)
		}
		d := int64(r - '0')
		if n > (math.MaxInt64-d)/10 {
			return 0, fmt.Errorf("%w: %q overflows", ErrMalformed, s)
		}
		n = n*10 + d
	}
	return n, nil
}

// String renders the amount with the currency's full precision, so that a
// value read back off the wire parses to the same integer it started as.
func (a Amount) String() string {
	sign := ""
	n := int64(a)
	if n < 0 {
		sign, n = "-", -n
	}
	return fmt.Sprintf("%s%d.%0*d", sign, n/scale, Decimals, n%scale)
}
