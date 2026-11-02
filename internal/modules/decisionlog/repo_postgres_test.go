//go:build integration

package decisionlog

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

// TestEnsurePartitions_CoversMonthsAheadAndIsIdempotent: the worker runs this
// daily, so a second run must change nothing, and a write months from now
// must find its partition waiting.
func TestEnsurePartitions_CoversMonthsAheadAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := fixtures.NewTestDB(t)
	svc := New(db.DB).Service()

	require.NoError(t, svc.EnsurePartitions(ctx, 14))
	require.NoError(t, svc.EnsurePartitions(ctx, 14))

	month := time.Now().UTC().AddDate(0, 14, 0)
	for _, parent := range []string{"decision", "decision_check"} {
		var exists bool
		name := fmt.Sprintf("%s_%04dm%02d", parent, month.Year(), int(month.Month()))
		require.NoError(t, db.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1)`, name).Scan(&exists))
		assert.True(t, exists, name)
	}
}
