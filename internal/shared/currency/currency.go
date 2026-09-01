// Package currency provides an exact decimal amount type paired with an asset.
//
// Note: never use float64 for money anywhere in this codebase. Binary floats
// cannot represent 0.40 exactly, so a ledger that must sum to zero will not.
package currency

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// Asset identifies the unit of account (e.g. USDC on Base).
type Asset string

const USDC Asset = "USDC"

// Amount is an exact decimal quantity of a specific Asset.
//
// There is deliberately no constructor taking float64. Binary floating point
// cannot represent 0.40, and the ledger invariant is exact equality with zero.
type Amount struct {
	value decimal.Decimal
	asset Asset
}

// Parse builds an Amount from its exact decimal string representation.
// `s` - string value representation
// `a` - `Asset` type
func Parse(s string, a Asset) (Amount, error) {
	if a == "" {
		return Amount{}, fmt.Errorf("currency: empty asset")
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return Amount{}, fmt.Errorf("currency: parse %q: %w", s, err)
	}
	return Amount{value: d, asset: a}, nil
}

// Zero returns an `Amount` struct with zero value
// `a` - `Asset` type
func Zero(a Asset) Amount {
	return Amount{value: decimal.Zero, asset: a}
}

// Asset method returns `Amount`'s asset
func (a Amount) Asset() Asset { return a.asset }

// String method returns `Amount`'s value in string format
func (a Amount) String() string { return a.value.String() }

// IsZero method checks if `Amount` value is zero
func (a Amount) IsZero() bool { return a.value.IsZero() }

// IsNegative method checks if `Amount` value is negative
func (a Amount) IsNegative() bool { return a.value.IsNegative() }

// Decimal method returns `Amount` value
func (a Amount) Decimal() decimal.Decimal { return a.value }

// Equal method compares 2 `Amount`s and return true if their assets and values equal, false otherwise
func (a Amount) Equal(b Amount) bool { return a.asset == b.asset && a.value.Equal(b.value) }

// GreaterThan method reports a > b. Callers must pass matching assets. Mismatched assets
// return an error.
func (a Amount) GreaterThan(b Amount) (bool, error) {
	if a.asset != b.asset {
		return false, fmt.Errorf("currency: asset mismatch %s vs %s", a.asset, b.asset)
	}
	return a.value.GreaterThan(b.value), nil
}

// Add method returns sum of `a` and `b`. Callers must pass matching assets. Mismatched assets
// return an error.
func (a Amount) Add(b Amount) (Amount, error) {
	if a.asset != b.asset {
		return Amount{}, fmt.Errorf("currency: asset mismatch %s vs %s", a.asset, b.asset)
	}
	return Amount{value: a.value.Add(b.value), asset: a.asset}, nil
}

// Neg returns the negation of a (e.g. 5 USDC becomes -5 USDC) as a new Amount.
// It does not modify a — Amount is immutable by convention; every method
// returns a new value rather than mutating the receiver.
func (a Amount) Neg() Amount { return Amount{value: a.value.Neg(), asset: a.asset} }

// Sum method adds amounts together, requiring every one to share asset.
// `asset` - Asset we use
// `amounts` - variadic parameter which accepts any number of `Amount` arguments (zero, one or more).
func Sum(asset Asset, amounts ...Amount) (Amount, error) {
	total := Zero(asset)
	for _, x := range amounts {
		var err error
		if total, err = total.Add(x); err != nil {
			return Amount{}, err
		}
	}
	return total, nil
}
