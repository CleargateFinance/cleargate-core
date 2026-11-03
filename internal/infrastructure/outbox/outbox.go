// Package outbox implements the transactional outbox pattern.
//
// Domain events are written to the outbox table in the SAME transaction as the
// state change that produced them. A relay publishes them afterwards. This
// gives an exactly-once-effective event stream without a distributed
// transaction, and it is the seam through which the analytics warehouse and
// any future extracted service will be fed.
//
// Only the writing half exists. The relay arrives with the first consumer;
// until then events accumulate unpublished, which costs rows, not facts.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
)

// Event is one fact to publish.
//
// Payload carries identifiers and amounts only, never personal data
// (data-architecture.md §8): the outbox feeds systems an erasure request
// cannot reach.
type Event struct {
	// AggregateType and AggregateID name the thing the event is about, for
	// example "payment" and its id.
	AggregateType string
	AggregateID   string

	// EventType is dotted and lower case, for example "payment.authorized".
	EventType string

	Payload any

	// OccurredAt is when the change happened, from the caller's clock, so it
	// agrees with the timestamps on the rows the change wrote.
	OccurredAt time.Time
}

// Writer appends events.
type Writer struct {
	db *database.DB
}

// NewWriter builds a Writer over db.
func NewWriter(db *database.DB) *Writer { return &Writer{db: db} }

// Append writes e. It joins the transaction open on ctx, which is the point:
// called inside the unit of work that made the change, the event commits
// exactly when the change does. Called outside one it commits on its own,
// which is only right for an event that reports nothing else.
func (w *Writer) Append(ctx context.Context, e Event) error {
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("outbox: encode %s payload: %w", e.EventType, err)
	}
	if _, err := w.db.Exec(ctx, `
		INSERT INTO event_outbox (aggregate_type, aggregate_id, event_type, payload, occurred_at)
		VALUES ($1, $2, $3, $4, $5)`,
		e.AggregateType, e.AggregateID, e.EventType, payload, e.OccurredAt,
	); err != nil {
		return fmt.Errorf("outbox: append %s: %w", e.EventType, err)
	}
	return nil
}
