// Package money provides an exact decimal amount type paired with an asset.
//
// Rule: never use float64 for money anywhere in this codebase. Binary floats
// cannot represent 0.40 exactly; a ledger that must sum to zero will not.
package money

// Amount is an exact decimal quantity of a specific Asset.
// TODO(scaffold): wrap shopspring/decimal; forbid construction from float64.
type Amount struct{}

// Asset identifies the unit of account (e.g. USDC on Base).
type Asset string

const USDC Asset = "USDC"
