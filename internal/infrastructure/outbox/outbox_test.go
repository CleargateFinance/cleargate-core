//go:build integration

package outbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

func event(at time.Time) Event {
	return Event{
		AggregateType: "payment",
		AggregateID:   "6f1c2b0e-5d1a-4e57-9a0e-3c0b6f2f6b11",
		EventType:     "payment.authorized",
		Payload:       map[string]string{"payment_id": "6f1c2b0e-5d1a-4e57-9a0e-3c0b6f2f6b11"},
		OccurredAt:    at,
	}
}

// TestAppend_CommitsAndRollsBackWithTheCallersTransaction: an event exists
// exactly when the change it reports committed.
func TestAppend_CommitsAndRollsBackWithTheCallersTransaction(t *testing.T) {
	ctx := context.Background()
	db := fixtures.NewTestDB(t)
	w := NewWriter(db.DB)
	at := time.Now().UTC().Truncate(time.Microsecond)

	failed := errors.New("the change failed")
	err := db.UoW.Do(ctx, func(ctx context.Context) error {
		require.NoError(t, w.Append(ctx, event(at)))
		return failed
	})
	require.ErrorIs(t, err, failed)

	require.NoError(t, db.UoW.Do(ctx, func(ctx context.Context) error {
		return w.Append(ctx, event(at))
	}))

	var count int
	var occurredAt time.Time
	var payload string
	require.NoError(t, db.QueryRow(ctx,
		`SELECT count(*), max(occurred_at), max(payload::text) FROM event_outbox`).Scan(&count, &occurredAt, &payload))
	assert.Equal(t, 1, count, "the rolled-back event must not exist")
	assert.True(t, at.Equal(occurredAt))
	assert.JSONEq(t, `{"payment_id":"6f1c2b0e-5d1a-4e57-9a0e-3c0b6f2f6b11"}`, payload)
}
