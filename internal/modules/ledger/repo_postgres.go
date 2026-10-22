package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// postgresRepo is the only place in this module that knows SQL exists.
//
// Every method runs through database.DB, which joins whatever transaction is
// open on the context, so none of these need to know whether they are part of
// a larger atomic operation.
type postgresRepo struct {
	db *database.DB
}

// NewPostgresRepository builds the Postgres-backed store.
func NewPostgresRepository(db *database.DB) Repository {
	return &postgresRepo{db: db}
}

// EnsureAccount returns an existing bucket or creates it.
//
// The insert is written as an upsert rather than a select-then-insert so two
// concurrent callers cannot both find nothing and both try to create the same
// bucket. DO NOTHING plus a follow-up select handles the loser of that race.
func (r *postgresRepo) EnsureAccount(
	ctx context.Context,
	accountID *id.AccountID,
	typ AccountType,
	asset currency.Asset,
) (id.LedgerAccountID, error) {
	const insert = `
		INSERT INTO ledger_account (account_id, type, asset)
		VALUES ($1, $2, $3)
		ON CONFLICT (account_id, type, asset) DO NOTHING
		RETURNING id`

	const selectExisting = `
		SELECT id FROM ledger_account
		WHERE account_id IS NOT DISTINCT FROM $1 AND type = $2 AND asset = $3`

	var owner any
	if accountID != nil {
		owner = string(*accountID)
	}

	var out string
	err := r.db.QueryRow(ctx, insert, owner, string(typ), string(asset)).Scan(&out)
	if err == nil {
		return id.LedgerAccountID(out), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("ledger: ensure account: %w", err)
	}

	// No row returned means the bucket already existed, so read it back.
	if err := r.db.QueryRow(ctx, selectExisting, owner, string(typ), string(asset)).Scan(&out); err != nil {
		return "", fmt.Errorf("ledger: load existing account: %w", err)
	}
	return id.LedgerAccountID(out), nil
}

// InsertEntry writes the entry header and one posting per line.
//
// The whole write runs inside one transaction, for two reasons. An entry
// without its postings would be a meaningless orphan, so the two must land
// together or not at all. And the balance check is deferred to commit, so
// every posting of an entry has to be inside the same transaction for the
// check to see the complete set. Inserted one statement at a time outside a
// transaction, the check would fire after the first line and always fail,
// since its counter-line would not exist yet.
//
// The unit of work joins an already open transaction rather than starting a
// second one, so a caller bundling this with other writes still gets a single
// atomic operation.
//
// Every posting is stamped with the entry's own timestamp. The foreign key
// requires that match, and it is what lets the balance check look in a single
// partition rather than scanning all of them.
func (r *postgresRepo) InsertEntry(ctx context.Context, e Entry) (id.JournalEntryID, error) {
	const insertEntry = `
		INSERT INTO journal_entry (kind, reference_type, reference_id, agent_id, reverses_entry_id, description, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at`

	// All the lines go in as one statement, so an entry costs two round trips
	// regardless of how many postings it has.
	const insertPostings = `
		INSERT INTO posting (journal_entry_id, ledger_account_id, amount, asset, created_at)
		SELECT $1, account::uuid, amount::numeric, $4, $5
		FROM unnest($2::text[], $3::text[]) AS lines(account, amount)`

	var referenceType any
	if e.ReferenceType != "" {
		referenceType = e.ReferenceType
	}
	var referenceID any
	if e.ReferenceID != nil {
		referenceID = string(*e.ReferenceID)
	}
	var agentID any
	if e.AgentID != nil {
		agentID = string(*e.AgentID)
	}
	var reverses any
	if e.Reverses != nil {
		reverses = string(*e.Reverses)
	}
	var description any
	if e.Description != "" {
		description = e.Description
	}

	accounts := make([]string, 0, len(e.Lines))
	amounts := make([]string, 0, len(e.Lines))
	for _, l := range e.Lines {
		accounts = append(accounts, string(l.LedgerAccountID))
		amounts = append(amounts, l.Amount.String())
	}

	var entryID string

	err := r.db.UoW.Do(ctx, func(ctx context.Context) error {
		if err := r.refuseOverdraft(ctx, e); err != nil {
			return err
		}

		var createdAt time.Time
		if err := r.db.QueryRow(ctx, insertEntry,
			string(e.Kind), referenceType, referenceID, agentID, reverses, description, e.OccurredAt,
		).Scan(&entryID, &createdAt); err != nil {
			return fmt.Errorf("ledger: insert entry: %w", err)
		}

		if _, err := r.db.Exec(ctx, insertPostings,
			entryID, accounts, amounts, string(e.Asset), createdAt,
		); err != nil {
			return fmt.Errorf("ledger: insert postings: %w", err)
		}

		return nil
	})
	if err != nil {
		return "", err
	}

	return id.JournalEntryID(entryID), nil
}

// refuseOverdraft returns ErrInsufficientFunds if the entry would take a
// customer funds bucket below zero. Debited buckets are locked first, so
// concurrent payments queue instead of spending the same balance.
//
// Lock and sum are separate statements so that, under READ COMMITTED, the sum
// sees what the previous lock holder committed. Platform buckets aren't
// checked: the on-chain float is negative by design.
func (r *postgresRepo) refuseOverdraft(ctx context.Context, e Entry) error {
	// id order avoids deadlocks. Unlike FOR UPDATE, NO KEY UPDATE doesn't block
	// the foreign key check of postings that only credit the bucket.
	const lockFunds = `
		SELECT id FROM ledger_account
		WHERE id = ANY($1::text[]::uuid[]) AND type = 'customer_funds'
		ORDER BY id
		FOR NO KEY UPDATE`

	const overdrawn = `
		SELECT EXISTS (
			SELECT 1
			FROM unnest($1::text[], $2::text[]) AS debit(account, amount)
			JOIN ledger_account la
			  ON la.id = debit.account::uuid AND la.type = 'customer_funds'
			WHERE debit.amount::numeric + (
				SELECT COALESCE(SUM(po.amount), 0)
				FROM posting po
				WHERE po.ledger_account_id = la.id AND po.asset = $3
			) < 0
		)`

	// Net the lines per bucket and keep the ones losing value.
	net := make(map[id.LedgerAccountID]decimal.Decimal, len(e.Lines))
	for _, l := range e.Lines {
		net[l.LedgerAccountID] = net[l.LedgerAccountID].Add(l.Amount.Decimal())
	}

	var accounts, amounts []string
	for account, amount := range net {
		if amount.IsNegative() {
			accounts = append(accounts, string(account))
			amounts = append(amounts, amount.String())
		}
	}
	if len(accounts) == 0 {
		return nil
	}

	if _, err := r.db.Exec(ctx, lockFunds, accounts); err != nil {
		return fmt.Errorf("ledger: lock customer funds: %w", err)
	}

	var refused bool
	if err := r.db.QueryRow(ctx, overdrawn, accounts, amounts, string(e.Asset)).Scan(&refused); err != nil {
		return fmt.Errorf("ledger: check customer funds: %w", err)
	}
	if refused {
		return ErrInsufficientFunds
	}
	return nil
}

// Balance sums every posting against one bucket.
func (r *postgresRepo) Balance(
	ctx context.Context,
	ledgerAccountID id.LedgerAccountID,
	asset currency.Asset,
) (currency.Amount, error) {
	const q = `
		SELECT COALESCE(SUM(amount), 0)
		FROM posting
		WHERE ledger_account_id = $1 AND asset = $2`

	var total decimal.Decimal
	if err := r.db.QueryRow(ctx, q, string(ledgerAccountID), string(asset)).Scan(&total); err != nil {
		return currency.Amount{}, fmt.Errorf("ledger: balance: %w", err)
	}
	return currency.Parse(total.String(), asset)
}

// BookTotal sums every posting for one asset, which must always be zero.
func (r *postgresRepo) BookTotal(ctx context.Context, asset currency.Asset) (currency.Amount, error) {
	const q = `SELECT COALESCE(SUM(amount), 0) FROM posting WHERE asset = $1`

	var total decimal.Decimal
	if err := r.db.QueryRow(ctx, q, string(asset)).Scan(&total); err != nil {
		return currency.Amount{}, fmt.Errorf("ledger: book total: %w", err)
	}
	return currency.Parse(total.String(), asset)
}

// IncrementSpend adds to a counter and returns the new total.
//
// This is one statement on purpose. Reading the current total into Go and
// writing back total plus amount would let two concurrent payments both read
// the same figure and both believe they fit under a cap. The upsert makes the
// read and the write a single indivisible operation.
func (r *postgresRepo) IncrementSpend(
	ctx context.Context,
	agentID id.AgentID,
	p Period,
	amount currency.Amount,
) (currency.Amount, error) {
	const q = `
		INSERT INTO agent_spend_counter (agent_id, period_type, period_start, asset, spent)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (agent_id, period_type, period_start, asset)
		DO UPDATE SET spent = agent_spend_counter.spent + EXCLUDED.spent,
		              updated_at = now()
		RETURNING spent`

	var total decimal.Decimal
	err := r.db.QueryRow(ctx, q,
		string(agentID), string(p.Type), p.Start, string(amount.Asset()), amount.Decimal(),
	).Scan(&total)
	if err != nil {
		return currency.Amount{}, fmt.Errorf("ledger: increment spend: %w", err)
	}
	return currency.Parse(total.String(), amount.Asset())
}

// DecrementSpend takes amount off a counter. It can't reuse IncrementSpend's
// upsert: Postgres checks spent >= 0 on the proposed insert row before
// resolving the conflict, so a negative amount always fails.
func (r *postgresRepo) DecrementSpend(
	ctx context.Context,
	agentID id.AgentID,
	p Period,
	amount currency.Amount,
) error {
	const q = `
		UPDATE agent_spend_counter
		SET spent = spent - $5, updated_at = now()
		WHERE agent_id = $1 AND period_type = $2 AND period_start = $3 AND asset = $4
		  AND spent >= $5`

	tag, err := r.db.Exec(ctx, q,
		string(agentID), string(p.Type), p.Start, string(amount.Asset()), amount.Decimal(),
	)
	if err != nil {
		return fmt.Errorf("ledger: decrement spend: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrReleaseExceedsSpend
	}
	return nil
}

// SpentIn reports a counter, treating a missing row as zero spent.
func (r *postgresRepo) SpentIn(
	ctx context.Context,
	agentID id.AgentID,
	p Period,
	asset currency.Asset,
) (currency.Amount, error) {
	const q = `
		SELECT COALESCE(SUM(spent), 0)
		FROM agent_spend_counter
		WHERE agent_id = $1 AND period_type = $2 AND period_start = $3 AND asset = $4`

	var total decimal.Decimal
	err := r.db.QueryRow(ctx, q, string(agentID), string(p.Type), p.Start, string(asset)).Scan(&total)
	if err != nil {
		return currency.Amount{}, fmt.Errorf("ledger: spent in period: %w", err)
	}
	return currency.Parse(total.String(), asset)
}

// StoredSpend lists every counter recorded for one window.
func (r *postgresRepo) StoredSpend(ctx context.Context, p Period) ([]Spend, error) {
	const q = `
		SELECT agent_id, asset, spent
		FROM agent_spend_counter
		WHERE period_type = $1 AND period_start = $2`

	rows, err := r.db.Query(ctx, q, string(p.Type), p.Start)
	if err != nil {
		return nil, fmt.Errorf("ledger: stored spend: %w", err)
	}
	defer rows.Close()

	return scanSpend(rows, p)
}

// RecomputeSpend derives what each agent's counter should be, from the
// postings themselves.
//
// Spending is the value that left a customer funds bucket on a payment entry
// attributed to that agent, less reversals of that payment. A reversal counts
// in the payment's window, matching ReleaseSpend. The sum is negated to give a
// positive figure comparable with the counter.
func (r *postgresRepo) RecomputeSpend(ctx context.Context, p Period) ([]Spend, error) {
	const q = `
		SELECT agent_id, asset, -SUM(amount)
		FROM (
			SELECT je.agent_id, po.asset, po.amount
			FROM posting po
			JOIN journal_entry je
			  ON je.id = po.journal_entry_id AND je.created_at = po.created_at
			JOIN ledger_account la
			  ON la.id = po.ledger_account_id
			WHERE je.agent_id IS NOT NULL
			  AND je.kind = 'payment'
			  AND la.type = 'customer_funds'
			  AND po.created_at >= $1
			  AND po.created_at < $2

			UNION ALL

			SELECT pay.agent_id, po.asset, po.amount
			FROM posting po
			JOIN journal_entry je
			  ON je.id = po.journal_entry_id AND je.created_at = po.created_at
			JOIN ledger_account la
			  ON la.id = po.ledger_account_id
			JOIN journal_entry pay
			  ON pay.id = je.reverses_entry_id AND pay.kind = 'payment'
			WHERE pay.agent_id IS NOT NULL
			  AND je.kind = 'reversal'
			  AND la.type = 'customer_funds'
			  AND pay.created_at >= $1
			  AND pay.created_at < $2
		) AS spend
		GROUP BY agent_id, asset`

	rows, err := r.db.Query(ctx, q, p.Start, p.End())
	if err != nil {
		return nil, fmt.Errorf("ledger: recompute spend: %w", err)
	}
	defer rows.Close()

	return scanSpend(rows, p)
}

// scanSpend reads rows of agent, asset and amount into Spend values.
func scanSpend(rows pgx.Rows, p Period) ([]Spend, error) {
	var out []Spend

	for rows.Next() {
		var agentID, asset string
		var amount decimal.Decimal

		if err := rows.Scan(&agentID, &asset, &amount); err != nil {
			return nil, fmt.Errorf("ledger: scan spend: %w", err)
		}

		parsed, err := currency.Parse(amount.String(), currency.Asset(asset))
		if err != nil {
			return nil, fmt.Errorf("ledger: parse spend amount: %w", err)
		}

		out = append(out, Spend{
			AgentID: id.AgentID(agentID),
			Period:  p,
			Asset:   currency.Asset(asset),
			Amount:  parsed,
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: iterate spend: %w", err)
	}
	return out, nil
}

// EnsurePartitions creates any missing monthly partitions.
//
// The work is done by a database function so the naming and range logic lives
// in one place, next to the tables it maintains, rather than being duplicated
// between the migration and this code.
func (r *postgresRepo) EnsurePartitions(ctx context.Context, monthsAhead int) error {
	const q = `SELECT ensure_ledger_partitions(1, $1)`

	if _, err := r.db.Exec(ctx, q, monthsAhead); err != nil {
		return fmt.Errorf("ledger: ensure partitions: %w", err)
	}
	return nil
}
