package settlement

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// postgresRepo is the only file in this module that knows SQL exists.
type postgresRepo struct {
	db *database.DB
}

// NewPostgresRepository builds the Postgres-backed store.
func NewPostgresRepository(db *database.DB) Repository {
	return &postgresRepo{db: db}
}

// Wallet implements Repository.
func (r *postgresRepo) Wallet(ctx context.Context, accountID id.AccountID, chain Chain) (Wallet, error) {
	w := Wallet{AccountID: accountID, Chain: chain}
	err := r.db.QueryRow(ctx,
		`SELECT provider, address FROM account_wallet WHERE account_id = $1 AND chain = $2`,
		string(accountID), string(chain),
	).Scan(&w.Provider, &w.Address)
	if errors.Is(err, pgx.ErrNoRows) {
		return Wallet{}, ErrNoWallet
	}
	if err != nil {
		return Wallet{}, fmt.Errorf("settlement: load wallet: %w", err)
	}
	return w, nil
}

// Save implements Repository. It is an insert that ignores the account's
// existing row rather than a select-then-insert, so two first calls cannot
// both write; the loser reads back the winner's. Only the primary key is
// ignored: an address already recorded for another account is a conflict
// on its own unique index and fails loudly.
//
// That index can also report the winner of a race: ON CONFLICT names only the
// primary key, and a row committing just as this insert passes that check
// fails on the address instead. So a violation is a real conflict only if the
// account still has no wallet of its own afterwards.
func (r *postgresRepo) Save(ctx context.Context, w Wallet) (Wallet, error) {
	_, err := r.db.Exec(ctx,
		`INSERT INTO account_wallet (account_id, chain, provider, address)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (account_id, chain) DO NOTHING`,
		string(w.AccountID), string(w.Chain), w.Provider, w.Address,
	)
	if err == nil {
		return r.Wallet(ctx, w.AccountID, w.Chain)
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "account_wallet_address_key" {
		if saved, readErr := r.Wallet(ctx, w.AccountID, w.Chain); readErr == nil {
			return saved, nil
		}
	}
	return Wallet{}, fmt.Errorf("settlement: save wallet: %w", err)
}
