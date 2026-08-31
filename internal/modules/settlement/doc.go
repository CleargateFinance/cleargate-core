// Package settlement turns authorized ledger positions into actual on-chain
// movement. Whitepaper §8.6.
//
// This is the one component deliberately isolated behind a queue rather than
// called inline (§8.1), because RPC unreliability must never enter the
// decision path. Authorization commits against the internal ledger; settlement
// happens asynchronously and in aggregate.
//
// Rail implementations sit behind a single adapter interface — the engine
// never learns what a chain is.
package settlement
