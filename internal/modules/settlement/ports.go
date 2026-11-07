package settlement

import (
	"context"

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
}
