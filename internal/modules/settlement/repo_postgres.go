package settlement

import (
	"context"
	"errors"
	"fmt"
	"time"

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

// Wallets implements Repository.
func (r *postgresRepo) Wallets(ctx context.Context, chain Chain) ([]Wallet, error) {
	rows, err := r.db.Query(ctx,
		`SELECT account_id, provider, address FROM account_wallet WHERE chain = $1 ORDER BY created_at`,
		string(chain))
	if err != nil {
		return nil, fmt.Errorf("settlement: list wallets: %w", err)
	}
	defer rows.Close()

	var out []Wallet
	for rows.Next() {
		w := Wallet{Chain: chain}
		var accountID string
		if err := rows.Scan(&accountID, &w.Provider, &w.Address); err != nil {
			return nil, fmt.Errorf("settlement: scan wallet: %w", err)
		}
		w.AccountID = id.AccountID(accountID)
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("settlement: list wallets: %w", err)
	}
	return out, nil
}

// DepositCursor implements Repository.
func (r *postgresRepo) DepositCursor(ctx context.Context, address string) (string, error) {
	var signature string
	err := r.db.QueryRow(ctx, `SELECT signature FROM deposit_cursor WHERE address = $1`, address).Scan(&signature)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("settlement: read deposit cursor: %w", err)
	}
	return signature, nil
}

// SaveDepositCursor implements Repository.
func (r *postgresRepo) SaveDepositCursor(ctx context.Context, address, signature string) error {
	if _, err := r.db.Exec(ctx, `
		INSERT INTO deposit_cursor (address, signature) VALUES ($1, $2)
		ON CONFLICT (address) DO UPDATE SET signature = EXCLUDED.signature, updated_at = now()`,
		address, signature,
	); err != nil {
		return fmt.Errorf("settlement: save deposit cursor: %w", err)
	}
	return nil
}

// DepositSeen implements Repository.
func (r *postgresRepo) DepositSeen(ctx context.Context, network, signature, address string) (bool, error) {
	var seen bool
	err := r.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM deposit WHERE network = $1 AND signature = $2 AND address = $3)`,
		network, signature, address).Scan(&seen)
	if err != nil {
		return false, fmt.Errorf("settlement: read deposit: %w", err)
	}
	return seen, nil
}

// InsertDeposit implements Repository. The primary key is what stops a second
// credit: a concurrent worker's insert waits here, then fails, and its whole
// transaction rolls back, credit included.
func (r *postgresRepo) InsertDeposit(ctx context.Context, d Deposit, at time.Time) error {
	var entry *string
	if d.JournalEntryID != nil {
		s := string(*d.JournalEntryID)
		entry = &s
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO deposit (network, signature, account_id, address, amount, asset, status, journal_entry_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		d.Network, d.Signature, string(d.AccountID), d.Address, d.Amount.Decimal(), string(d.Amount.Asset()),
		string(d.Status), entry, at,
	)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "deposit_pkey" {
		return errDepositSeen
	}
	if err != nil {
		return fmt.Errorf("settlement: insert deposit: %w", err)
	}
	return nil
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
