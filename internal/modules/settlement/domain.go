package settlement

import (
	"errors"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
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

// Token is the one token that is money on the rail: USDC, on one network (§8.3).
// Its decimals are configuration, never read from anything a merchant sends.
type Token struct {
	// Symbol is what moves, as payment.token records it.
	Symbol string
	// Network is a CAIP-2 chain id.
	Network string
	// Mint is the token's address on Network.
	Mint string
	// Decimals is how many atomic units make one token, as a power of ten.
	Decimals int
}

// DepositAddress is where an account's owner sends money, and what to send.
type DepositAddress struct {
	Address string
	Token   Token
}

// DepositStatus is what became of tokens that arrived.
type DepositStatus string

const (
	// DepositCredited moved the ledger.
	DepositCredited DepositStatus = "credited"
	// DepositQuarantined arrived past the account's funding cap. The tokens
	// are in the wallet and the balance does not count them.
	DepositQuarantined DepositStatus = "quarantined"
)

// Deposit is tokens arriving in an account's wallet, recorded once.
type Deposit struct {
	Network   string
	Signature string
	AccountID id.AccountID
	Address   string

	// Amount is in the unit of account, at par.
	Amount currency.Amount
	Status DepositStatus

	// JournalEntryID is the entry that credited it, nil when quarantined.
	JournalEntryID *id.JournalEntryID
}

// errDepositSeen is a deposit already recorded, by an earlier poll or another
// worker.
var errDepositSeen = errors.New("settlement: deposit already recorded")

// ErrNoWallet reports an account with no wallet on the chain asked about.
var ErrNoWallet = apperr.NotFound("the account has no wallet")

// errWalletMismatch reports a provider answering with a different address
// from the one already recorded for the account, which means the provider's
// records and ours disagree, for example a CDP project swapped underneath a
// database. Nothing may be signed or deposited until a person looks.
var errWalletMismatch = apperr.Internal("settlement: provider and records disagree on the account's wallet", nil)
