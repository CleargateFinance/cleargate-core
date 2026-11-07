package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/outbox"
	"github.com/CleargateFinance/cleargate-core/internal/modules/authz"
	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// maxAttempts bounds how often Authorize re-decides after losing a race. One
// retry normally settles it, since the second decision sees the winner's
// spend and declines; the bound only stops a pathological loop.
const maxAttempts = 3

var (
	// errRaceLost is a concurrent payment taking the funds or budget this
	// decision counted on, between the read and the write. The transaction
	// rolls back and Authorize decides again on fresh inputs.
	errRaceLost = errors.New("payment: a concurrent payment changed the inputs")

	// errKeyTaken is a concurrent request with the same key committing first.
	errKeyTaken = errors.New("payment: idempotency key stored by a concurrent request")
)

// Service implements this module's use cases. Depends on the interfaces
// declared in ports.go, never on repo_postgres.go or handler_http.go.
type Service struct {
	repo      Repository
	uow       database.UnitOfWork
	accounts  Accounts
	mandates  Mandates
	payees    Counterparties
	ledger    Ledger
	decisions DecisionLog
	events    Outbox
	clock     Clock
	rail      Rail
	transfers Transfers
}

// Pay decides a merchant's requirement and, if it is approved, signs it.
//
// The decision commits first and the signature follows, because the custodian
// is a network call and must not sit inside the transaction or the 100ms budget
// (§6.6). If signing fails the payment stays authorized with nothing handed out:
// a retry with the same key signs again, and it is safe to, since nobody holds
// the first. An agent that never retries gets its funds back from the settlement
// sweep (Task 7.7).
func (s *Service) Pay(ctx context.Context, req PayRequest) (Paid, error) {
	quote, err := s.transfers.Quote(req.Requirement)
	if err != nil {
		return Paid{}, err
	}

	signals := req.Signals
	if signals.ResourceURL == "" {
		signals.ResourceURL = quote.ResourceURL
	}
	res, err := s.Authorize(ctx, Request{
		AccountID:      req.AccountID,
		AgentID:        req.AgentID,
		IdempotencyKey: req.IdempotencyKey,
		Amount:         quote.Amount,
		Category:       req.Category,
		PayTo:          quote.PayTo,
		Signals:        signals,
		Terms:          quote.Terms,
	})
	if err != nil {
		return Paid{}, err
	}
	if res.Status != StatusAuthorized {
		return Paid{Result: res}, nil
	}
	return s.handOver(ctx, req, res)
}

// handOver returns the payment's signed transaction, signing it if nobody has.
//
// It reads the payment back rather than trusting the result it was given: on a
// replay the key remembers "authorized" for a day, while the payment itself may
// have settled or expired since, and an expired transaction must not be handed
// out as if it could still pay.
func (s *Service) handOver(ctx context.Context, req PayRequest, res Result) (Paid, error) {
	recorded, err := s.repo.AgentPayments(ctx, req.AgentID, []id.PaymentID{res.PaymentID})
	if err != nil {
		return Paid{}, err
	}
	if len(recorded) != 1 {
		return Paid{}, apperr.Internal("payment: an approved payment is not readable", nil)
	}
	payment := recorded[0]
	res.Status = payment.Status
	if res.Status == StatusExpired || res.Status == StatusFailed {
		return Paid{Result: res}, nil
	}

	stored, found, err := s.repo.StoredTransfer(ctx, res.PaymentID)
	if err != nil {
		return Paid{}, err
	}
	if found {
		return Paid{Result: res, PaymentSignature: stored.PaymentSignature}, nil
	}

	signed, err := s.transfers.Transfer(ctx, TransferRequest{
		AccountID:   req.AccountID,
		PaymentID:   res.PaymentID,
		Amount:      payment.Amount,
		PayTo:       payment.PayTo,
		Requirement: req.Requirement,
	})
	if err != nil {
		return Paid{}, err
	}

	saved, err := s.repo.SaveTransfer(ctx, res.PaymentID, signed, s.clock.Now().UTC())
	if errors.Is(err, errNoLongerAuthorized) {
		// The sweep released it while this was signing, so the money is back
		// and this transaction must never leave. Nobody has seen it.
		res.Status = StatusExpired
		return Paid{Result: res}, nil
	}
	if err != nil {
		return Paid{}, err
	}
	return Paid{Result: res, PaymentSignature: saved.PaymentSignature}, nil
}

// Authorize decides one payment and records the decision.
//
// Reads happen first, outside any transaction, then the pure engine decides,
// then every write commits in one transaction: the payment, its idempotency
// key, the ledger debit and spend counters when approved, the decision record
// and the outbox event. If any of them fails, none of them happened, so money
// never moves without its evidence (§5.4). A decline is recorded like an
// approval, minus the money.
//
// Errors are only for requests that cannot be judged: malformed input, an
// agent that is gone, or a failure in the system itself. None of those is
// remembered against the key, so a retry is evaluated afresh (Task 5.2).
func (s *Service) Authorize(ctx context.Context, req Request) (Result, error) {
	if err := req.Validate(); err != nil {
		return Result{}, err
	}
	hash := req.hash()

	for attempt := 1; ; attempt++ {
		res, err := s.authorizeOnce(ctx, req, hash)
		switch {
		case errors.Is(err, errKeyTaken):
			// The concurrent request's result is now committed, so answer
			// with it, exactly as a later retry would be answered.
			res, found, err := s.remembered(ctx, req, hash, s.clock.Now().UTC())
			if err == nil && !found {
				err = apperr.Internal("payment: idempotency key taken but not readable", nil)
			}
			return res, err
		case errors.Is(err, errRaceLost) && attempt < maxAttempts:
			continue
		case errors.Is(err, errRaceLost):
			return Result{}, ErrBusy
		default:
			return res, err
		}
	}
}

func (s *Service) authorizeOnce(ctx context.Context, req Request, hash []byte) (Result, error) {
	// One instant for the whole decision: the payment, the decision, the
	// posting, the counters and the key all carry it, so a later release
	// lands in the same counter windows (Task 7.7). start keeps the monotonic
	// reading that UTC() strips, so latency survives a wall-clock step.
	start := s.clock.Now()
	now := start.UTC()

	if res, found, err := s.remembered(ctx, req, hash, now); err != nil || found {
		return res, err // §8.4: a retry is free and changes nothing
	}

	// --- Gather. Every read happens before the decision, none inside it.
	agent, err := s.accounts.Agent(ctx, req.AgentID)
	if err != nil {
		return Result{}, err
	}
	if agent.AccountID != req.AccountID {
		// The credential named one account and the agent belongs to another.
		// Authentication produced both, so this is a defect, never a request.
		return Result{}, apperr.Internal("payment: agent does not belong to the authenticated account", nil)
	}
	if !agent.Active {
		return Result{}, ErrAgentRevoked
	}
	blocked, err := s.accounts.SpendingBlocked(ctx, req.AccountID)
	if err != nil {
		return Result{}, err
	}
	mandate, err := s.mandates.Current(ctx, req.AgentID)
	if err != nil {
		return Result{}, err
	}
	// Counters and balance in the unit of account, whatever the request says.
	// A request in another asset is the engine's to decline.
	day, month, err := s.ledger.Spent(ctx, req.AgentID, currency.USD, now)
	if err != nil {
		return Result{}, err
	}
	balance, err := s.ledger.Balance(ctx, req.AccountID, currency.USD)
	if err != nil {
		return Result{}, err
	}
	// Resolved outside the transaction, so a counterparty can outlive a
	// payment that then fails. It was still observed: an agent tried to pay
	// it, and the directory is built from exactly that (Task 5.5).
	payee, err := s.payees.Resolve(ctx, Payee{
		ResourceURL: req.Signals.ResourceURL,
		Network:     s.rail.Network,
		PayTo:       req.PayTo,
	})
	if err != nil {
		return Result{}, err
	}

	snap := authz.Snapshot{
		AccountBlocked: blocked,
		DaySpent:       day,
		MonthSpent:     month,
		AccountBalance: balance,
		Now:            now,
	}
	if mandate != nil {
		rules := mandate.Rules
		snap.Mandate = &rules
	}

	// --- Decide. Pure, no I/O.
	d := authz.Decide(req.authz(), snap)

	features, err := json.Marshal(d.Features)
	if err != nil {
		return Result{}, apperr.Internal("payment: encode features", err)
	}
	res := Result{
		Status:     StatusDeclined,
		Outcome:    d.Outcome,
		ReasonCode: d.ReasonCode,
		Reason:     d.Reason,
		DecidedAt:  now,
	}
	if d.Approved() {
		res.Status = StatusAuthorized
	}
	latency := max(s.clock.Now().Sub(start), 0)

	// --- Persist. One transaction, all or nothing.
	err = s.uow.Do(ctx, func(ctx context.Context) error {
		paymentID, err := s.repo.InsertPayment(ctx, Payment{
			AccountID:   req.AccountID,
			AgentID:     req.AgentID,
			Amount:      req.Amount,
			Category:    req.Category,
			PayTo:       req.PayTo,
			ResourceURL: req.Signals.ResourceURL,
			Rail:        s.rail,
			Status:      res.Status,
			DecidedAt:   now,

			CounterpartyID: payee,
		})
		if err != nil {
			return err
		}
		res.PaymentID = paymentID

		// The key goes in before any money moves. A concurrent request with
		// the same key waits here for this one to commit and then gives up,
		// so it never takes a lock on the account's funds.
		response, err := encodeResult(res)
		if err != nil {
			return apperr.Internal("payment: encode result", err)
		}
		if err := s.repo.Remember(ctx, RememberedKey{
			AccountID:   req.AccountID,
			Key:         req.IdempotencyKey,
			RequestHash: hash,
			PaymentID:   paymentID,
			Response:    response,
			StoredAt:    now,
		}, now.Add(-KeyRetention)); err != nil {
			return err
		}

		if d.Approved() {
			if err := s.charge(ctx, req, mandate, paymentID, now); err != nil {
				return err
			}
		}

		if err := s.decisions.Record(ctx, DecisionRecord{
			PaymentID:   paymentID,
			AccountID:   req.AccountID,
			AgentID:     req.AgentID,
			Decision:    d,
			Features:    features,
			Mandate:     mandate,
			EvaluatedAt: now,
			Latency:     latency,
		}); err != nil {
			return err
		}

		return s.events.Append(ctx, event(req, res))
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

// charge debits the account and counts the spend, inside the transaction.
//
// The reads before Decide were a snapshot, and two kinds of change can land
// after it. A freeze, a revocation or a new mandate version is re-checked
// first, with the rows held until commit, so the change either waits for this
// payment or this payment sees it. A concurrent payment spending the same
// funds or budget is caught by the writes themselves, which are atomic and
// report the state after them (§8.5): the debit refuses to overdraw, and a
// counter past its cap means the snapshot was stale. Any of these rolls the
// transaction back, and Authorize decides again on fresh inputs, which then
// records the decline.
func (s *Service) charge(ctx context.Context, req Request, m *Mandate, paymentID id.PaymentID, now time.Time) error {
	spendable, err := s.accounts.HoldSpending(ctx, req.AccountID, req.AgentID)
	if err != nil {
		return err
	}
	current, err := s.mandates.HoldCurrent(ctx, req.AgentID, m.ID)
	if err != nil {
		return err
	}
	if !spendable || !current {
		return errRaceLost
	}

	funded, err := s.ledger.Debit(ctx, Debit{
		PaymentID: paymentID,
		AccountID: req.AccountID,
		AgentID:   req.AgentID,
		Amount:    req.Amount,
		At:        now,
	})
	if err != nil {
		return err
	}
	if !funded {
		return errRaceLost
	}

	day, month, err := s.ledger.RecordSpend(ctx, req.AgentID, req.Amount, now)
	if err != nil {
		return err
	}
	for _, c := range []struct{ total, limit currency.Amount }{
		{day, m.Rules.MaxPerDay},
		{month, m.Rules.MaxPerMonth},
	} {
		over, err := c.total.GreaterThan(c.limit)
		if err != nil {
			return apperr.Internal("payment: compare spend with cap", err)
		}
		if over {
			return errRaceLost
		}
	}
	return nil
}

// remembered returns the stored answer for this request's key, if it is
// still within retention at now. A stored answer for a different request is
// a conflict.
//
// now must be the instant Remember will expire keys against, or a key that
// expires between the two reads is neither replayed nor replaced.
func (s *Service) remembered(ctx context.Context, req Request, hash []byte, now time.Time) (Result, bool, error) {
	k, found, err := s.repo.Remembered(ctx, req.AccountID, req.IdempotencyKey, now.Add(-KeyRetention))
	if err != nil || !found {
		return Result{}, false, err
	}
	if !bytes.Equal(k.RequestHash, hash) {
		return Result{}, false, ErrKeyReused
	}
	res, err := decodeResult(k.Response)
	if err != nil {
		return Result{}, false, err
	}
	res.Replayed = true
	return res, true, nil
}

// event reports the decision. Identifiers and amounts only, never the
// agent's free text (data-architecture.md §8).
func event(req Request, res Result) outbox.Event {
	eventType := "payment.declined"
	if res.Status == StatusAuthorized {
		eventType = "payment.authorized"
	}
	return outbox.Event{
		AggregateType: "payment",
		AggregateID:   string(res.PaymentID),
		EventType:     eventType,
		Payload: map[string]string{
			"payment_id":  string(res.PaymentID),
			"account_id":  string(req.AccountID),
			"agent_id":    string(req.AgentID),
			"reason_code": string(res.ReasonCode),
			"amount":      req.Amount.String(),
			"asset":       string(req.Amount.Asset()),
		},
		OccurredAt: res.DecidedAt,
	}
}

// Retention deletes idempotency keys past KeyRetention. It is separate from
// Service so the worker can run it without wiring the payment path's ports.
type Retention struct {
	repo  Repository
	clock Clock
}

// ExpireKeys deletes keys past retention, reporting how many. Authorize
// already ignores an expired key, so this only keeps the table small.
func (r *Retention) ExpireKeys(ctx context.Context) (int64, error) {
	return r.repo.DeleteKeysBefore(ctx, r.clock.Now().Add(-KeyRetention))
}

// Lookup reads payments already recorded: for the decision log, what each
// decision was about, and for the account summary, how many were declined. Separate from Service for the reason Retention is:
// reading a payment needs none of the payment path's ports.
//
// It has no actor check. Its caller has already established that the person
// asking may read this agent's records.
type Lookup struct {
	repo Repository
}

// AgentPayments returns the agent's payments among ids. An id that is not
// one of this agent's payments is left out rather than reported.
func (l *Lookup) AgentPayments(ctx context.Context, agentID id.AgentID, ids []id.PaymentID) ([]Recorded, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return l.repo.AgentPayments(ctx, agentID, ids)
}

// DeclinesSince totals the account's declines decided at or after since, per
// agent and asset: the console's "blocked 14 payments" (§12.1).
func (l *Lookup) DeclinesSince(ctx context.Context, accountID id.AccountID, since time.Time) ([]DeclineTotal, error) {
	return l.repo.DeclinesSince(ctx, accountID, since)
}
