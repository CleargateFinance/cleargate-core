package mandate

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

// postgresRepo is the only place in this module that knows SQL exists.
type postgresRepo struct {
	db *database.DB
}

// NewPostgresRepository builds the Postgres-backed store.
func NewPostgresRepository(db *database.DB) Repository {
	return &postgresRepo{db: db}
}

// mandateColumns is what every read returns. The projection columns are left
// out on purpose: the service parses the rules from the payload.
const mandateColumns = `
	id, agent_id, version, payload, payload_digest, signature_scheme,
	signed_by, status, effective_from, effective_to, created_at`

// IssueVersion supersedes the active mandate and inserts the next version.
//
// Concurrency is settled by the unique indexes rather than by a lock taken
// up front. Two issues racing for one agent both read the same latest version,
// and whichever commits second collides with the first on (agent_id, version)
// or on the one-active index, and is reported as ErrConcurrentChange with
// nothing written. Neither waits on a lock it might hold while the other
// needs it, and neither silently replaces rules it never saw.
//
// The superseded mandate's end is never set before its own start. Instances
// whose clocks disagree by a few milliseconds would otherwise fail the
// table's ordering check on an ordinary reissue.
func (r *postgresRepo) IssueVersion(
	ctx context.Context,
	agentID id.AgentID,
	at time.Time,
	build func(version int) (Draft, error),
) (Mandate, error) {
	const latest = `SELECT COALESCE(MAX(version), 0) FROM mandate WHERE agent_id = $1`

	const supersede = `
		UPDATE mandate
		SET status = 'superseded', effective_to = GREATEST($2, effective_from)
		WHERE agent_id = $1 AND status = 'active'`

	const insert = `
		INSERT INTO mandate (agent_id, version, payload, signature_scheme,
		                     signed_by, status, effective_from)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING ` + mandateColumns

	var out Mandate
	err := r.db.UoW.Do(ctx, func(ctx context.Context) error {
		var version int
		if err := r.db.QueryRow(ctx, latest, string(agentID)).Scan(&version); err != nil {
			return fmt.Errorf("mandate: latest version: %w", err)
		}

		draft, err := build(version + 1)
		if err != nil {
			return err
		}

		if _, err := r.db.Exec(ctx, supersede, string(agentID), at); err != nil {
			return fmt.Errorf("mandate: supersede: %w", err)
		}

		out, err = scanMandate(r.db.QueryRow(ctx, insert,
			string(draft.AgentID), draft.Version, draft.Payload, string(draft.Scheme),
			string(draft.SignedBy), string(draft.Status), draft.EffectiveFrom,
		))
		if err != nil {
			return fmt.Errorf("mandate: insert: %w", err)
		}
		return nil
	})
	if isUniqueViolation(err) {
		return Mandate{}, ErrConcurrentChange
	}
	if err != nil {
		return Mandate{}, err
	}
	return out, nil
}

// Active returns the mandate in force now.
func (r *postgresRepo) Active(ctx context.Context, agentID id.AgentID) (Mandate, error) {
	const q = `SELECT ` + mandateColumns + `
		FROM mandate
		WHERE agent_id = $1 AND status = 'active'`

	m, err := scanMandate(r.db.QueryRow(ctx, q, string(agentID)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Mandate{}, ErrNoMandate
	}
	if err != nil {
		return Mandate{}, fmt.Errorf("mandate: load active: %w", err)
	}
	return m, nil
}

// ActiveAt returns the mandate whose validity interval contains at.
//
// Only active and superseded mandates ever had an interval in force. A pending
// one has not taken effect and a vetoed one never will, so neither can be the
// answer whatever its dates say. Ordering by version settles the overlap of a
// few milliseconds that clock skew can leave between two versions, in favour of
// the newer one.
func (r *postgresRepo) ActiveAt(ctx context.Context, agentID id.AgentID, at time.Time) (Mandate, error) {
	const q = `SELECT ` + mandateColumns + `
		FROM mandate
		WHERE agent_id = $1
		  AND status IN ('active', 'superseded')
		  AND effective_from <= $2
		  AND (effective_to IS NULL OR effective_to > $2)
		ORDER BY version DESC
		LIMIT 1`

	m, err := scanMandate(r.db.QueryRow(ctx, q, string(agentID), at))
	if errors.Is(err, pgx.ErrNoRows) {
		return Mandate{}, ErrNoMandate
	}
	if err != nil {
		return Mandate{}, fmt.Errorf("mandate: load active at: %w", err)
	}
	return m, nil
}

// scanMandate reads one row selected with mandateColumns.
func scanMandate(row pgx.Row) (Mandate, error) {
	var (
		m        Mandate
		rawID    string
		agentID  string
		scheme   string
		signedBy string
		status   string
	)
	err := row.Scan(
		&rawID, &agentID, &m.Version, &m.Payload, &m.Digest, &scheme,
		&signedBy, &status, &m.EffectiveFrom, &m.EffectiveTo, &m.CreatedAt,
	)
	if err != nil {
		return Mandate{}, err
	}

	m.ID = id.MandateID(rawID)
	m.AgentID = id.AgentID(agentID)
	m.Scheme = SignatureScheme(scheme)
	m.SignedBy = id.UserID(signedBy)
	m.Status = Status(status)
	return m, nil
}

// isUniqueViolation reports whether err is Postgres rejecting a duplicate.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
