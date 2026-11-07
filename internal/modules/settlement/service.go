package settlement

import (
	"context"
	"errors"
	"fmt"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Service implements this module's use cases. Depends on the interfaces
// declared in ports.go, never on repo_postgres.go or handler_http.go.
type Service struct {
	repo     Repository
	wallets  WalletProvider
	accounts Accounts
	token    Token
}

// NewService builds a Service.
func NewService(repo Repository, wallets WalletProvider) *Service {
	return &Service{repo: repo, wallets: wallets}
}

// Address returns the account's wallet address, creating the wallet on first
// use (Task 7.5). An account that never asks for an address never costs a
// wallet.
//
// The provider is asked only when nothing is recorded, so after the first
// call this is one indexed read. Two first calls at once both reach the
// provider, which returns the same address to both (WalletProvider.Create is
// idempotent), and both saves leave one row.
func (s *Service) Address(ctx context.Context, accountID id.AccountID) (string, error) {
	w, err := s.repo.Wallet(ctx, accountID, s.wallets.Chain())
	if err == nil {
		return w.Address, nil
	}
	if !errors.Is(err, ErrNoWallet) {
		return "", err
	}

	address, err := s.wallets.Create(ctx, accountID)
	if err != nil {
		return "", fmt.Errorf("settlement: create wallet: %w", err)
	}
	saved, err := s.repo.Save(ctx, Wallet{
		AccountID: accountID, Chain: s.wallets.Chain(), Provider: s.wallets.Name(), Address: address,
	})
	if err != nil {
		return "", err
	}
	if saved.Address != address {
		return "", errWalletMismatch
	}
	return saved.Address, nil
}

// DepositAddress is where a member of the account sends money. Any member may
// ask, a viewer included: it is the account's public address, and asking is
// what creates the wallet on an account's first deposit.
func (s *Service) DepositAddress(ctx context.Context, userID id.UserID, accountID id.AccountID) (DepositAddress, error) {
	if err := s.accounts.RequireMember(ctx, userID, accountID); err != nil {
		return DepositAddress{}, err
	}
	address, err := s.Address(ctx, accountID)
	if err != nil {
		return DepositAddress{}, err
	}
	return DepositAddress{Address: address, Token: s.token}, nil
}

// SignTransaction signs tx with the account's wallet. It never creates one:
// an account with no wallet has never been given an address to fund, so it
// has nothing to pay with, and signing for it would mean a wallet nobody can
// reconcile against.
func (s *Service) SignTransaction(ctx context.Context, accountID id.AccountID, tx []byte) ([]byte, error) {
	w, err := s.repo.Wallet(ctx, accountID, s.wallets.Chain())
	if err != nil {
		return nil, err
	}
	signed, err := s.wallets.Sign(ctx, w.Address, tx)
	if err != nil {
		return nil, fmt.Errorf("settlement: sign: %w", err)
	}
	return signed, nil
}
