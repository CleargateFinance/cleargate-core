//go:build integration

package account

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

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
