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
//
// Account wallets (Task 7.5): each account has one wallet per chain, never
// one per agent, because the money is pooled per account and agents hold only
// spend counters (§9.3). The provider holds the keys (§8.8: no private keys
// in-house) behind WalletProvider; this module records only the address, and
// the database refuses a second wallet for an account, an address shared by
// two accounts, and any change to a wallet once recorded.
//
// Deposits (Phase 8): DepositWatcher reads each wallet's history at finality
// and credits what arrived. A deposit is keyed by transaction and wallet, so
// reading the same history twice credits nothing twice. Address screening is
// not wired yet; see the Revision notes before this reaches real money.
package settlement
