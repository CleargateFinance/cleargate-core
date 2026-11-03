//go:build integration

package counterparty

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

func TestResolve_OneIdentityIsOneCounterparty(t *testing.T) {
	ctx := context.Background()
	db := fixtures.NewTestDB(t)
	svc := New(db.DB, nil).Service()

	a, err := svc.Resolve(ctx, Observation{ResourceURL: "https://api.weather.com/v1/forecast?city=paris"})
	require.NoError(t, err)
	b, err := svc.Resolve(ctx, Observation{ResourceURL: "HTTPS://API.WEATHER.COM/v1/forecast/"})
	require.NoError(t, err)
	c, err := svc.Resolve(ctx, Observation{Network: devnet, PayTo: "7xKX"})
	require.NoError(t, err)

	require.NotNil(t, a)
	assert.Equal(t, *a, *b)
	assert.NotEqual(t, *a, *c)

	var key, domain string
	require.NoError(t, db.QueryRow(ctx,
		`SELECT canonical_key, domain FROM counterparty WHERE id = $1`, string(*a)).Scan(&key, &domain))
	assert.Equal(t, "endpoint:api.weather.com/v1/forecast", key, "the database derives the key Go looks it up by")
	assert.Equal(t, "weather.com", domain)
}

func TestResolve_NothingToIdentifyIsNil(t *testing.T) {
	db := fixtures.NewTestDB(t)

	cp, err := New(db.DB, nil).Service().Resolve(context.Background(), Observation{})

	require.NoError(t, err)
	assert.Nil(t, cp)
}

// TestResolve_ConcurrentFirstPaymentsShareOneCounterparty: ten agents finding
// the same new endpoint at once create it once.
func TestResolve_ConcurrentFirstPaymentsShareOneCounterparty(t *testing.T) {
	db := fixtures.NewTestDB(t)
	svc := New(db.DB, nil).Service()

	const n = 10
	ids := make([]*id.CounterpartyID, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], errs[i] = svc.Resolve(context.Background(), Observation{ResourceURL: "https://new.example.com/x"})
		}()
	}
	wg.Wait()

	for i := range n {
		require.NoError(t, errs[i])
		assert.Equal(t, *ids[0], *ids[i])
	}
	var count int
	require.NoError(t, db.QueryRow(context.Background(), `SELECT count(*) FROM counterparty`).Scan(&count))
	assert.Equal(t, 1, count)
}

// TestCounterparty_SchemaRefusesInconsistentRows writes rows directly, the way
// a script or a later code path would, to show what the table refuses alone.
func TestCounterparty_SchemaRefusesInconsistentRows(t *testing.T) {
	ctx := context.Background()
	db := fixtures.NewTestDB(t)

	cases := map[string]string{
		"endpoint without host": `INSERT INTO counterparty (kind, domain, path, first_seen_at)
			VALUES ('endpoint', 'example.com', '/x', now())`,
		"endpoint with an address": `INSERT INTO counterparty (kind, domain, host, path, network, address, first_seen_at)
			VALUES ('endpoint', 'example.com', 'example.com', '/x', '` + devnet + `', '7xKX', now())`,
		"address without network": `INSERT INTO counterparty (kind, address, first_seen_at)
			VALUES ('address', '7xKX', now())`,
		"upper-case host": `INSERT INTO counterparty (kind, domain, host, path, first_seen_at)
			VALUES ('endpoint', 'example.com', 'API.example.com', '/x', now())`,
		"relative path": `INSERT INTO counterparty (kind, domain, host, path, first_seen_at)
			VALUES ('endpoint', 'example.com', 'example.com', 'x', now())`,
		"claim without account": `INSERT INTO counterparty (kind, domain, host, path, claimed_at, first_seen_at)
			VALUES ('endpoint', 'example.com', 'example.com', '/x', now(), now())`,
		"unknown kind": `INSERT INTO counterparty (kind, network, address, first_seen_at)
			VALUES ('account', '` + devnet + `', '7xKX', now())`,
	}
	for name, insert := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := db.Exec(ctx, insert)
			assert.Error(t, err)
		})
	}

	_, err := db.Exec(ctx, `INSERT INTO counterparty (kind, domain, host, path, first_seen_at)
		VALUES ('endpoint', 'example.com', 'example.com', '/x', now())`)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `INSERT INTO counterparty (kind, domain, host, path, first_seen_at)
		VALUES ('endpoint', 'example.com', 'example.com', '/x', now())`)
	assert.Error(t, err, "one identity is one counterparty")
}

func TestCounterparty_IdentityIsImmutableAndClaimedOnce(t *testing.T) {
	ctx := context.Background()
	db := fixtures.NewTestDB(t)
	cp, err := New(db.DB, nil).Service().Resolve(ctx, Observation{ResourceURL: "https://example.com/x"})
	require.NoError(t, err)

	var accountID, otherAccountID string
	for _, target := range []*string{&accountID, &otherAccountID} {
		require.NoError(t, db.QueryRow(ctx, `
			WITH p AS (INSERT INTO principal (kind) VALUES ('legal_entity') RETURNING id)
			INSERT INTO account (principal_id, display_name) SELECT id, 'payee' FROM p
			RETURNING id`).Scan(target))
	}

	_, err = db.Exec(ctx, `UPDATE counterparty SET path = '/y' WHERE id = $1`, string(*cp))
	assert.Error(t, err, "identity is immutable")
	_, err = db.Exec(ctx, `UPDATE counterparty SET first_seen_at = now() - interval '1 year' WHERE id = $1`, string(*cp))
	assert.Error(t, err, "history is not rewritten")
	_, err = db.Exec(ctx, `DELETE FROM counterparty WHERE id = $1`, string(*cp))
	assert.Error(t, err, "a counterparty is never deleted")

	_, err = db.Exec(ctx, `UPDATE counterparty SET account_id = $2, claimed_at = $3 WHERE id = $1`,
		string(*cp), accountID, time.Now())
	require.NoError(t, err, "a payee claims its record")
	_, err = db.Exec(ctx, `UPDATE counterparty SET account_id = $2 WHERE id = $1`, string(*cp), otherAccountID)
	assert.Error(t, err, "and claims it once")
}
