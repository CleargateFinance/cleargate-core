package decisionlog

import (
	"context"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Service implements this module's use cases. It depends on the interfaces in
// ports.go, never on the storage implementation directly.
type Service struct {
	repo Repository
}

// NewService builds a Service.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// Record stores one decision. Call it inside the unit of work that writes the
// payment: the database refuses to commit a payment without its decision, and
// a decision cannot be written without its payment, so the two land together.
//
// The hash chain of Task 5.4 is not built yet; prev_hash, hash and signature
// stay null until it is.
func (s *Service) Record(ctx context.Context, r Record) (id.DecisionID, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	return s.repo.Insert(ctx, r)
}

// EnsurePartitions creates any monthly partitions that do not exist yet.
func (s *Service) EnsurePartitions(ctx context.Context, monthsAhead int) error {
	return s.repo.EnsurePartitions(ctx, monthsAhead)
}

// Log reads the decision log for the console (§10.1): every payment an agent
// attempted, the declines above all, since they exist nowhere else.
//
// Separate from Service, which writes on the payment path, so the worker and
// the payment path need not wire who may read.
type Log struct {
	repo     Repository
	accounts Accounts
	payments Payments
}

// NewLog builds a Log. Both ports are required; Module.New refuses a nil one.
func NewLog(repo Repository, accounts Accounts, payments Payments) *Log {
	return &Log{repo: repo, accounts: accounts, payments: payments}
}

// AgentDecisions returns one page of an agent's decisions, newest first.
//
// Any member of the agent's account may read it, a viewer included: reading
// changes nothing, and the log is what a viewer is there to see. A revoked
// agent's log stays readable, since its history is still somebody's to
// answer for.
//
// Paging is by cursor, not offset. Decisions arrive while the console reads,
// and an offset would shift under every new one, repeating entries on the
// next page.
func (l *Log) AgentDecisions(ctx context.Context, actor Actor, agentID id.AgentID, q Query) (Page, error) {
	if err := q.Validate(); err != nil {
		return Page{}, err
	}
	if err := l.accounts.RequireAgentReader(ctx, actor, agentID); err != nil {
		return Page{}, err
	}

	limit := q.Limit
	if limit == 0 {
		limit = DefaultPageSize
	}
	// One more than the page, so the last page is known without a second
	// query, and no client is handed a cursor that leads to nothing.
	q.Limit = limit + 1
	entries, err := l.repo.AgentDecisions(ctx, agentID, q)
	if err != nil {
		return Page{}, err
	}

	var page Page
	if len(entries) > limit {
		entries = entries[:limit]
		last := entries[limit-1]
		page.Next = &Cursor{EvaluatedAt: last.EvaluatedAt, ID: last.ID}
	}

	ids := make([]id.PaymentID, len(entries))
	for i, e := range entries {
		ids[i] = e.PaymentID
	}
	payments, err := l.payments.AgentPayments(ctx, agentID, ids)
	if err != nil {
		return Page{}, err
	}
	for i := range entries {
		p, ok := payments[entries[i].PaymentID]
		if !ok {
			// A decision's foreign key names its payment and agent together,
			// so this is a defect, not an absence to show as blank.
			return Page{}, apperr.Internal("decisionlog: decision "+string(entries[i].ID)+" has no payment", nil)
		}
		entries[i].Payment = p
	}

	page.Entries = entries
	return page, nil
}
