// Package decisionlog records every evaluated payment — approvals, holds and
// especially declines. Whitepaper §4.3, §8.7, §10.1.
//
// This package is the product's core asset, not a logging concern:
//
//   - It is the evidence that makes the guarantee credible in a dispute.
//   - The declines and holds are what the console sells (§10.1) — they exist
//     nowhere else, since approvals are visible on-chain.
//   - Its feature snapshots are the only possible training data for the
//     supervised models of §6.5. Features not captured at decision time cannot
//     be reconstructed later at any price.
//
// Records are immutable, signed, and hash-chained to the previous record.
package decisionlog
