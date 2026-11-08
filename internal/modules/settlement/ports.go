package settlement

import (
	"context"
	"math/big"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// WalletProvider holds account wallets' keys and signs with them (§8.8). It is
// the only thing in the codebase that knows which custodian is used, so
// replacing CDP, or adding a user-controlled model beside its Server Wallets,
// changes the implementation and nothing that calls it.
type WalletProvider interface {
	// Chain is the chain the provider's wallets live on.
	Chain() Chain

	// Name identifies the provider in the records, such as "cdp".
	Name() string

	// Create returns the address of the account's wallet, creating the
	// wallet if the provider has none for the account. It must be
	// idempotent per account: two calls, even concurrent ones or a retry
	// after a lost response, return the same address.
	Create(ctx context.Context, accountID id.AccountID) (string, error)

	// Sign signs a serialized transaction with the key behind address and
	// returns the transaction with the signature added. It signs whatever it
	// is given: checking the transaction against the decision is the
	// caller's job (Task 7.6).
	Sign(ctx context.Context, address string, tx []byte) ([]byte, error)
}

// Rail builds and signs one payment (§8.6). It is the seam a second scheme or a
// second chain arrives through, so nothing above it knows what a chain is. The
// MVP's only implementation is x402 `exact` on Solana (Task 7.6).
type Rail interface {
	Transfer(ctx context.Context, t RailTransfer) (RailSigned, error)
}

// RailTransfer is one transfer to build and sign.
type RailTransfer struct {
	// Owner is the wallet paying, and FeePayer who pays the fee and submits.
	Owner, FeePayer string

	Network, Mint, PayTo string

	// AtomicAmount is exactly what the merchant asked for, in atomic units.
	AtomicAmount string

	// Memo goes on-chain, and is how the transaction is found again.
	Memo string

	// Sign adds Owner's signature to a serialized transaction.
	Sign func(ctx context.Context, tx []byte) ([]byte, error)
}

// RailSigned is a signed transfer, not yet on-chain.
type RailSigned struct {
	Transaction          string
	LastValidBlockHeight uint64

	// Message is the bytes every signer signs, which the chain takes once.
	Message []byte
}

// ChainReader reads the chain the wallets live on, for the configured token,
// at finality: nothing here can be rolled back after the ledger has moved.
type ChainReader interface {
	// TokenAccount is the address that holds the token for owner, which is
	// where a transfer to owner lands.
	TokenAccount(owner string) (string, error)

	// Signatures returns one page of the transactions touching address, newest
	// first.
	Signatures(ctx context.Context, address string, p ChainPage) ([]ChainSignature, error)

	// Transaction reads one transaction, with what it did to owner's holding of
	// the token.
	Transaction(ctx context.Context, signature, owner string) (ChainTransaction, error)

	// FinalizedHeight is how far the chain has finalized.
	FinalizedHeight(ctx context.Context) (uint64, error)

	// MessageOf is the message of a signed transaction, in the form
	// ChainTransaction.Message reports it.
	MessageOf(signedTransaction string) ([]byte, error)

	// TokenBalance is how much of the token owner holds, in atomic units, zero
	// when it has no token account. A failed read is an error, never zero.
	TokenBalance(ctx context.Context, owner string) (*big.Int, error)
}

// ChainPage selects a range of an address's history.
type ChainPage struct {
	Before, Until string
	Limit         int
}

// ChainSignature is one transaction in an address's history.
type ChainSignature struct {
	Signature string
	Failed    bool

	// Memo is how the chain renders the transaction's memos, so match it with
	// strings.Contains.
	Memo string

	// BlockTime is zero when the node does not know it.
	BlockTime time.Time
}

// ChainTransaction is what one transaction did.
type ChainTransaction struct {
	// Message is the bytes every signer signed, which identifies a transaction
	// whose id Cleargate never saw.
	Message []byte

	Failed bool

	// TokenChange is how much of the token the owner gained, in atomic units,
	// negative for a loss.
	TokenChange *big.Int
}

// FundingLedger is what the deposit watcher needs from the ledger.
type FundingLedger interface {
	// CreditFunding moves amount from the on-chain float to the account's
	// funds, joining the transaction open on ctx, and returns the entry.
	CreditFunding(ctx context.Context, accountID id.AccountID, amount currency.Amount, at time.Time) (id.JournalEntryID, error)
}

// FundsLedger is what the wallet reconciler needs from the ledger.
type FundsLedger interface {
	// CustomerFunds is what the ledger holds for the account, in the unit of
	// account: the balance it has promised the customer.
	CustomerFunds(ctx context.Context, accountID id.AccountID) (currency.Amount, error)
}

// FundingGate decides whether money may be accepted.
type FundingGate interface {
	// FundingAllowed reports false when amount would take the account past its
	// funding cap. Any other failure is an error, and nothing is decided.
	//
	// The cap is a total across the owner's accounts, so it holds a lock on
	// that owner until the transaction on ctx ends. Call it inside the
	// transaction that credits: outside one, a concurrent credit to another of
	// the owner's accounts is not yet counted.
	FundingAllowed(ctx context.Context, accountID id.AccountID, amount currency.Amount) (bool, error)
}

// Clock supplies the current time, so tests control "now" without sleeping.
type Clock interface {
	Now() time.Time
}

// Accounts is what this module needs from the account module.
type Accounts interface {
	// RequireMember refuses a person outside the account, with the answer
	// every account route gives one.
	RequireMember(ctx context.Context, userID id.UserID, accountID id.AccountID) error
}

// Repository is the storage this module needs.
type Repository interface {
	// Wallet returns the account's wallet on chain, or ErrNoWallet.
	Wallet(ctx context.Context, accountID id.AccountID, chain Chain) (Wallet, error)

	// Save records w unless the account already has a wallet on its chain,
	// and returns whichever wallet is recorded afterwards, so the loser of
	// two concurrent first calls reads back the winner's.
	Save(ctx context.Context, w Wallet) (Wallet, error)

	// Wallets returns every wallet on chain.
	Wallets(ctx context.Context, chain Chain) ([]Wallet, error)

	// DepositCursor is the newest signature already handled for address, empty
	// when none is.
	DepositCursor(ctx context.Context, address string) (string, error)

	// SaveDepositCursor records the newest signature handled for address.
	SaveDepositCursor(ctx context.Context, address, signature string) error

	// DepositSeen reports whether this deposit is already recorded.
	DepositSeen(ctx context.Context, network, signature, address string) (bool, error)

	// InsertDeposit records a deposit, joining the transaction open on ctx, or
	// reports errDepositSeen.
	InsertDeposit(ctx context.Context, d Deposit, at time.Time) error
}
