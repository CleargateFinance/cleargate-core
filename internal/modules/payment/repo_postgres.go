package payment

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

// InsertPayment writes the payment row. A decline is final at once, so it is
// resolved at its decision instant; an authorization stays open for the chain.
func (r *postgresRepo) InsertPayment(ctx context.Context, p Payment) (id.PaymentID, error) {
	var resolvedAt any
	if p.Status == StatusDeclined {
		resolvedAt = p.DecidedAt
	}

	var out string
	err := r.db.QueryRow(ctx, `
		INSERT INTO payment (account_id, agent_id, amount, asset, category, pay_to, resource_url,
		                     rail, token, network, status, decided_at, resolved_at, counterparty_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING id`,
		string(p.AccountID), string(p.AgentID), p.Amount.Decimal(), string(p.Amount.Asset()),
		nullable(p.Category), nullable(p.PayTo), nullable(p.ResourceURL),
		p.Rail.Name, p.Rail.Token, p.Rail.Network,
		string(p.Status), p.DecidedAt, resolvedAt, p.CounterpartyID,
	).Scan(&out)
	if err != nil {
		return "", fmt.Errorf("payment: insert payment: %w", err)
	}
	return id.PaymentID(out), nil
}

// Remembered reads a key stored at or after since. An older row may still be
// there until the worker deletes it, and is treated as gone.
func (r *postgresRepo) Remembered(
	ctx context.Context, accountID id.AccountID, key string, since time.Time,
) (RememberedKey, bool, error) {
	k := RememberedKey{AccountID: accountID, Key: key}
	var paymentID string
	err := r.db.QueryRow(ctx, `
		SELECT request_hash, payment_id, response, created_at
		FROM idempotency_key
		WHERE account_id = $1 AND key = $2 AND created_at >= $3`,
		string(accountID), key, since,
	).Scan(&k.RequestHash, &paymentID, &k.Response, &k.StoredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RememberedKey{}, false, nil
	}
	if err != nil {
		return RememberedKey{}, false, fmt.Errorf("payment: read idempotency key: %w", err)
	}
	k.PaymentID = id.PaymentID(paymentID)
	return k, true, nil
}

// Remember stores a key. An expired row under the same key is deleted first:
// rows are immutable, so a new payment under an old key is a new row, not an
// update. The primary key makes a concurrent insert of the same key wait for
// this transaction, then fail, which is what reports errKeyTaken to it.
func (r *postgresRepo) Remember(ctx context.Context, k RememberedKey, expiredBefore time.Time) error {
	if _, err := r.db.Exec(ctx, `
		DELETE FROM idempotency_key
		WHERE account_id = $1 AND key = $2 AND created_at < $3`,
		string(k.AccountID), k.Key, expiredBefore,
	); err != nil {
		return fmt.Errorf("payment: clear expired idempotency key: %w", err)
	}

	_, err := r.db.Exec(ctx, `
		INSERT INTO idempotency_key (account_id, key, request_hash, payment_id, response, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		string(k.AccountID), k.Key, k.RequestHash, string(k.PaymentID), k.Response, k.StoredAt,
	)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "idempotency_key_pkey" {
		return errKeyTaken
	}
	if err != nil {
		return fmt.Errorf("payment: store idempotency key: %w", err)
	}
	return nil
}

// DeleteKeysBefore deletes expired keys.
func (r *postgresRepo) DeleteKeysBefore(ctx context.Context, before time.Time) (int64, error) {
	tag, err := r.db.Exec(ctx, `DELETE FROM idempotency_key WHERE created_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("payment: delete expired idempotency keys: %w", err)
	}
	return tag.RowsAffected(), nil
}

// nullable stores an empty string as NULL: not sent is not the same as sent
// empty, and the columns are nullable for exactly that.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
