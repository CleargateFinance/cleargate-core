//go:build integration

package account

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

// TestFreezeAndSuspensionAreIndependent: a customer's freeze and a platform
// suspension must not share state, or unfreezing would lift a suspension.
func TestFreezeAndSuspensionAreIndependent(t *testing.T) {
	ctx := context.Background()
	db := fixtures.NewTestDB(t)

	var accountID string
	require.NoError(t, db.QueryRow(ctx, `
		WITH p AS (INSERT INTO principal (kind) VALUES ('person') RETURNING id)
		INSERT INTO account (principal_id, display_name)
		SELECT id, 'test' FROM p
		RETURNING id`).Scan(&accountID))

	_, err := db.Exec(ctx, `UPDATE account SET frozen_at = now() WHERE id = $1`, accountID)
	require.NoError(t, err, "an active account can be frozen")

	_, err = db.Exec(ctx, `UPDATE account SET status = 'suspended', frozen_at = NULL WHERE id = $1`, accountID)
	require.NoError(t, err, "a suspended account can be unfrozen")
}

// TestSpendingBlocked_UnlessActiveAndUnfrozen: either a freeze or a
// suspension stops spending, and only an active, unfrozen account may spend.
func TestSpendingBlocked_UnlessActiveAndUnfrozen(t *testing.T) {
	ctx := context.Background()
	db := fixtures.NewTestDB(t)
	repo := NewPostgresRepository(db.DB)

	var accountID string
	require.NoError(t, db.QueryRow(ctx, `
		WITH p AS (INSERT INTO principal (kind) VALUES ('person') RETURNING id)
		INSERT INTO account (principal_id, display_name)
		SELECT id, 'test' FROM p
		RETURNING id`).Scan(&accountID))

	states := []struct {
		name    string
		update  string
		blocked bool
	}{
		{"active", `UPDATE account SET status = 'active', frozen_at = NULL WHERE id = $1`, false},
		{"frozen", `UPDATE account SET frozen_at = now() WHERE id = $1`, true},
		{"suspended", `UPDATE account SET status = 'suspended', frozen_at = NULL WHERE id = $1`, true},
	}
	for _, s := range states {
		_, err := db.Exec(ctx, s.update, accountID)
		require.NoError(t, err)

		blocked, err := repo.SpendingBlocked(ctx, id.AccountID(accountID))
		require.NoError(t, err)
		require.Equal(t, s.blocked, blocked, s.name)
	}

	_, err := repo.SpendingBlocked(ctx, id.AccountID("00000000-0000-0000-0000-000000000000"))
	require.ErrorIs(t, err, ErrAccountNotFound)
}
