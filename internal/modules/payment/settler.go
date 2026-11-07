package payment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/outbox"
)

const (
	// UnsignedExpiry is how long an authorized payment with no transaction
	// waits before its funds and budget are given back. Signing failed and the
	// agent did not retry. It is far longer than a retry takes, and nothing can
	// land meanwhile, because no transaction exists.
	UnsignedExpiry = 10 * time.Minute

	// sweepBatch bounds one sweep. The oldest come first, so a backlog clears
	// rather than starving the payments at its head.
	sweepBatch = 500
)

// Settler moves authorized payments to their outcome (Task 7.7). It is separate
// from Service so the worker can run it without the payment path's ports or a
// custodian: reading the chain signs nothing.
type Settler struct {
	repo   Repository
	uow    database.UnitOfWork
	ledger SettlementLedger
	chain  TransferOutcomes
	events Outbox
	clock  Clock
	log    *slog.Logger
}

// SettlerDeps are what a Settler needs. All are required but Clock and Log.
type SettlerDeps struct {
	DB     *database.DB
	Ledger SettlementLedger
	Chain  TransferOutcomes
	Outbox Outbox
	Clock  Clock
	Log    *slog.Logger
}

// NewSettler builds a Settler against Postgres.
func NewSettler(d SettlerDeps) (*Settler, error) {
	switch {
	case d.DB == nil:
		return nil, fmt.Errorf("%w: database", ErrMissingDependency)
	case d.Ledger == nil:
		return nil, fmt.Errorf("%w: ledger", ErrMissingDependency)
	case d.Chain == nil:
		return nil, fmt.Errorf("%w: chain", ErrMissingDependency)
	case d.Outbox == nil:
		return nil, fmt.Errorf("%w: outbox", ErrMissingDependency)
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Settler{
		repo: NewPostgresRepository(d.DB), uow: d.DB.UoW, ledger: d.Ledger, chain: d.Chain,
		events: d.Outbox, clock: clockOrSystem(d.Clock), log: log,
	}, nil
}

// Sweep resolves every authorized payment whose outcome is now known. A failure
// on one does not stop the others, and what it could not resolve waits for the
// next sweep: every step here can safely run again.
func (s *Settler) Sweep(ctx context.Context) error {
	pending, err := s.repo.AwaitingSettlement(ctx, sweepBatch)
	if err != nil {
		return err
	}

	var signed []Pending
	var failures []error
	now := s.clock.Now()
	for _, p := range pending {
		switch {
		case p.Transfer != nil:
			signed = append(signed, p)
		case now.Sub(p.DecidedAt) >= UnsignedExpiry:
			if err := s.resolve(ctx, p, StatusExpired); err != nil {
				failures = append(failures, fmt.Errorf("payment %s: %w", p.ID, err))
			}
		}
	}
	if len(signed) == 0 {
		return errors.Join(failures...)
	}

	// Any outcomes it did read are still acted on when part of the read failed.
	outcomes, err := s.chain.Outcomes(ctx, signed)
	if err != nil {
		failures = append(failures, err)
	}
	for _, p := range signed {
		status, known := outcomes[p.ID]
		if !known || status == StatusAuthorized {
			continue
		}
		if err := s.resolve(ctx, p, status); err != nil {
			failures = append(failures, fmt.Errorf("payment %s: %w", p.ID, err))
		}
	}
	return errors.Join(failures...)
}

// resolve records one outcome: the status, its ledger entry and its event, all
// or none (§5.4).
//
// The row lock comes first, and each check after it is its own statement, so
// under READ COMMITTED it sees what committed while waiting. That is what makes
// this safe against a request signing the same payment: migration 000011's
// trigger takes a share lock on this row, so whichever arrives second sees the
// first. A transaction is therefore never stored for a released payment, and a
// payment with a stored transaction is never expired as unsigned.
func (s *Settler) resolve(ctx context.Context, p Pending, status Status) error {
	at := s.clock.Now().UTC()
	if at.Before(p.DecidedAt) {
		// The database refuses to resolve a payment before it was decided, and
		// the two instants can come from different workers' clocks.
		at = p.DecidedAt
	}

	return s.uow.Do(ctx, func(ctx context.Context) error {
		authorized, err := s.repo.LockAuthorized(ctx, p.ID)
		if err != nil || !authorized {
			return err
		}
		if p.Transfer == nil {
			_, signedSince, err := s.repo.StoredTransfer(ctx, p.ID)
			if err != nil || signedSince {
				return err
			}
		}

		if status == StatusSettled {
			err = s.ledger.Settle(ctx, p, at)
		} else {
			err = s.ledger.Release(ctx, p, at)
		}
		if err != nil {
			return err
		}
		if err := s.repo.Resolve(ctx, p.ID, status, at); err != nil {
			return err
		}
		s.log.Info("payment resolved", slog.String("payment_id", string(p.ID)), slog.String("status", string(status)))
		return s.events.Append(ctx, outbox.Event{
			AggregateType: "payment",
			AggregateID:   string(p.ID),
			EventType:     "payment." + string(status),
			Payload: map[string]string{
				"payment_id": string(p.ID),
				"account_id": string(p.AccountID),
				"agent_id":   string(p.AgentID),
				"amount":     p.Amount.String(),
				"asset":      string(p.Amount.Asset()),
			},
			OccurredAt: at,
		})
	})
}
