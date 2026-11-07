package payment

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
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

// AgentPayments reads a page's worth of payments by primary key. The agent is
// in the filter as well as the ids, so a caller that passed the wrong ids
// still sees only this agent's payments.
func (r *postgresRepo) AgentPayments(ctx context.Context, agentID id.AgentID, ids []id.PaymentID) ([]Recorded, error) {
	keys := make([]string, len(ids))
	for i, p := range ids {
		keys[i] = string(p)
	}

	rows, err := r.db.Query(ctx, `
		SELECT id, amount, asset, COALESCE(category, ''), COALESCE(pay_to, ''),
		       COALESCE(resource_url, ''), status
		FROM payment
		WHERE id = ANY($1::uuid[]) AND agent_id = $2`, keys, string(agentID))
	if err != nil {
		return nil, fmt.Errorf("payment: read payments: %w", err)
	}
	defer rows.Close()

	var out []Recorded
	for rows.Next() {
		var p Recorded
		var paymentID, asset, status string
		var amount decimal.Decimal
		if err := rows.Scan(&paymentID, &amount, &asset, &p.Category, &p.PayTo, &p.ResourceURL, &status); err != nil {
			return nil, fmt.Errorf("payment: scan payment: %w", err)
		}
		if p.Amount, err = currency.Parse(amount.String(), currency.Asset(asset)); err != nil {
			return nil, fmt.Errorf("payment: parse payment amount: %w", err)
		}
		p.ID, p.Status = id.PaymentID(paymentID), Status(status)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("payment: read payments: %w", err)
	}
	return out, nil
}

// StoredTransfer reads a payment's signed transaction.
func (r *postgresRepo) StoredTransfer(ctx context.Context, paymentID id.PaymentID) (Transfer, bool, error) {
	var t Transfer
	var height int64
	err := r.db.QueryRow(ctx, `
		SELECT memo, wallet_address, signed_transaction, payment_signature, last_valid_block_height
		FROM payment_transfer WHERE payment_id = $1`, string(paymentID),
	).Scan(&t.Memo, &t.WalletAddress, &t.Transaction, &t.PaymentSignature, &height)
	if errors.Is(err, pgx.ErrNoRows) {
		return Transfer{}, false, nil
	}
	if err != nil {
		return Transfer{}, false, fmt.Errorf("payment: read transfer: %w", err)
	}
	t.LastValidBlockHeight = uint64(height) //nolint:gosec // The column is checked positive.
	return t, true, nil
}

// SaveTransfer inserts a payment's signed transaction. The primary key settles
// two concurrent signings, and the insert trigger refuses a payment the sweep
// has already released (migration 000011).
func (r *postgresRepo) SaveTransfer(ctx context.Context, paymentID id.PaymentID, t Transfer, at time.Time) (Transfer, error) {
	if t.LastValidBlockHeight > math.MaxInt64 {
		return Transfer{}, fmt.Errorf("payment: block height %d does not fit", t.LastValidBlockHeight)
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO payment_transfer (payment_id, memo, wallet_address, signed_transaction, message_digest,
		                              payment_signature, last_valid_block_height, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (payment_id) DO NOTHING`,
		string(paymentID), t.Memo, t.WalletAddress, t.Transaction, t.MessageDigest, t.PaymentSignature,
		int64(t.LastValidBlockHeight), at, //nolint:gosec // Bounded above.
	)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "CG001" {
		return Transfer{}, errNoLongerAuthorized
	}
	// ON CONFLICT names only the primary key, so two retries of this payment
	// signing the same message can meet on the digest instead. The message is
	// another payment's only if this one still has no transfer of its own.
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "payment_transfer_message_digest_key" {
		stored, found, readErr := r.StoredTransfer(ctx, paymentID)
		if readErr != nil {
			return Transfer{}, readErr
		}
		if found {
			return stored, nil
		}
		return Transfer{}, ErrTransactionInFlight
	}
	if err != nil {
		return Transfer{}, fmt.Errorf("payment: save transfer: %w", err)
	}

	stored, found, err := r.StoredTransfer(ctx, paymentID)
	if err != nil {
		return Transfer{}, err
	}
	if !found {
		return Transfer{}, fmt.Errorf("payment: transfer for %s not readable after saving", paymentID)
	}
	return stored, nil
}

// AwaitingSettlement reads what the sweep has to resolve, through the partial
// index migration 000007 keeps for exactly this.
func (r *postgresRepo) AwaitingSettlement(ctx context.Context, limit int) ([]Pending, error) {
	rows, err := r.db.Query(ctx, `
		SELECT p.id, p.account_id, p.agent_id, p.amount, p.asset, p.decided_at,
		       t.memo, t.wallet_address, t.signed_transaction, t.payment_signature, t.last_valid_block_height
		FROM payment p
		LEFT JOIN payment_transfer t ON t.payment_id = p.id
		WHERE p.status = 'authorized'
		ORDER BY p.decided_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("payment: read pending payments: %w", err)
	}
	defer rows.Close()

	var out []Pending
	for rows.Next() {
		var p Pending
		var paymentID, accountID, agentID, asset string
		var amount decimal.Decimal
		var memo, wallet, transaction, signature *string
		var height *int64
		if err := rows.Scan(&paymentID, &accountID, &agentID, &amount, &asset, &p.DecidedAt,
			&memo, &wallet, &transaction, &signature, &height); err != nil {
			return nil, fmt.Errorf("payment: scan pending payment: %w", err)
		}
		if p.Amount, err = currency.Parse(amount.String(), currency.Asset(asset)); err != nil {
			return nil, fmt.Errorf("payment: parse pending amount: %w", err)
		}
		p.ID, p.AccountID, p.AgentID = id.PaymentID(paymentID), id.AccountID(accountID), id.AgentID(agentID)
		if memo != nil {
			p.Transfer = &Transfer{
				Memo: *memo, WalletAddress: *wallet, Transaction: *transaction, PaymentSignature: *signature,
				LastValidBlockHeight: uint64(*height), //nolint:gosec // The column is checked positive.
			}
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("payment: read pending payments: %w", err)
	}
	return out, nil
}

// LockAuthorized takes the row lock the sweep's decision rests on.
func (r *postgresRepo) LockAuthorized(ctx context.Context, paymentID id.PaymentID) (bool, error) {
	var status string
	err := r.db.QueryRow(ctx, `SELECT status FROM payment WHERE id = $1 FOR UPDATE`, string(paymentID)).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("payment: lock payment: %w", err)
	}
	return Status(status) == StatusAuthorized, nil
}

// Resolve writes a payment's final status. payment_guard permits only
// authorized to an outcome, so a defect here fails rather than rewrites history.
func (r *postgresRepo) Resolve(ctx context.Context, paymentID id.PaymentID, status Status, at time.Time) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE payment SET status = $2, resolved_at = $3 WHERE id = $1 AND status = 'authorized'`,
		string(paymentID), string(status), at)
	if err != nil {
		return fmt.Errorf("payment: resolve payment: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("payment: %s was not authorized when resolved", paymentID)
	}
	return nil
}

// DeclinesSince groups by agent and asset in one statement. A decline can be
// in any asset the agent asked for (asset_not_permitted is one), so the sums
// stay apart rather than being added across assets.
func (r *postgresRepo) DeclinesSince(ctx context.Context, accountID id.AccountID, since time.Time) ([]DeclineTotal, error) {
	rows, err := r.db.Query(ctx, `
		SELECT agent_id, asset, count(*), COALESCE(sum(amount) FILTER (WHERE amount > 0), 0)
		FROM payment
		WHERE account_id = $1 AND status = 'declined' AND decided_at >= $2
		GROUP BY agent_id, asset
		ORDER BY agent_id, asset`, string(accountID), since)
	if err != nil {
		return nil, fmt.Errorf("payment: total declines: %w", err)
	}
	defer rows.Close()

	var out []DeclineTotal
	for rows.Next() {
		var t DeclineTotal
		var agentID, asset string
		var amount decimal.Decimal
		if err := rows.Scan(&agentID, &asset, &t.Count, &amount); err != nil {
			return nil, fmt.Errorf("payment: scan decline total: %w", err)
		}
		if t.Amount, err = currency.Parse(amount.String(), currency.Asset(asset)); err != nil {
			return nil, fmt.Errorf("payment: parse decline total: %w", err)
		}
		t.AgentID = id.AgentID(agentID)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("payment: total declines: %w", err)
	}
	return out, nil
}

// nullable stores an empty string as NULL: not sent is not the same as sent
// empty, and the columns are nullable for exactly that.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
