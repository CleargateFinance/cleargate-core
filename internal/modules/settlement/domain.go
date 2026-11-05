package settlement

import (
	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Chain names the network family a wallet lives on. Solana launches first,
// then Base (AGENTS.md), each with its own wallet for the same account.
type Chain string

// ChainSolana is the only chain the MVP settles on.
const ChainSolana Chain = "solana"

// Wallet is an account's on-chain address on one chain. The key behind it is
// the provider's; the address is all this module keeps.
type Wallet struct {
	AccountID id.AccountID
	Chain     Chain

	// Provider names who holds the key, such as "cdp".
	Provider string

	// Address is the public address, exactly as the chain spells it.
	Address string
}

// ErrNoWallet reports an account with no wallet on the chain asked about.
var ErrNoWallet = apperr.NotFound("the account has no wallet")

// errWalletMismatch reports a provider answering with a different address
// from the one already recorded for the account, which means the provider's
// records and ours disagree, for example a CDP project swapped underneath a
// database. Nothing may be signed or deposited until a person looks.
var errWalletMismatch = apperr.Internal("settlement: provider and records disagree on the account's wallet", nil)
