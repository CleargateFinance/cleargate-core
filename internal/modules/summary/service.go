package summary

import (
	"context"

	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Service implements this module's use cases. It depends on the interfaces in
// ports.go and nothing else.
type Service struct {
	accounts Accounts
	ledger   Ledger
	mandates Mandates
	payments Payments
	clock    Clock
}

// NewService builds a Service. Every port is required; Module.New refuses a
// nil one.
func NewService(accounts Accounts, ledger Ledger, mandates Mandates, payments Payments, clock Clock) *Service {
	return &Service{accounts: accounts, ledger: ledger, mandates: mandates, payments: payments, clock: clock}
}

// Account summarises one account for a person who belongs to it.
//
// Membership is checked first and nothing else is read for somebody who
// fails it. The figures are separate reads, not one snapshot: a payment
// committing meanwhile can show in the balance and not yet in a counter. That
// is a display a refresh corrects, and holding every module's rows still for
// a console page would slow the payment path to make it exact.
//
// It reads the counters and the mandate once per agent. An account has a
// handful of agents; when one has hundreds, the ports grow a batch form.
func (s *Service) Account(ctx context.Context, actor Actor, accountID id.AccountID) (Summary, error) {
	account, err := s.accounts.Membership(ctx, actor, accountID)
	if err != nil {
		return Summary{}, err
	}
	agents, err := s.accounts.Agents(ctx, actor, accountID)
	if err != nil {
		return Summary{}, err
	}

	now := s.clock.Now()
	sum := Summary{Account: account, Agents: agents, Declined: Declines{Amounts: []currency.Amount{}}}
	sum.DayStart, sum.MonthStart = s.ledger.Windows(now)

	if sum.Balance, err = s.ledger.Balance(ctx, accountID); err != nil {
		return Summary{}, err
	}
	declines, err := s.payments.DeclinesSince(ctx, accountID, sum.MonthStart)
	if err != nil {
		return Summary{}, err
	}

	for i := range sum.Agents {
		a := &sum.Agents[i]
		if a.Mandate, err = s.mandates.Current(ctx, a.ID); err != nil {
			return Summary{}, err
		}
		if a.SpentToday, a.SpentThisMonth, err = s.ledger.Spent(ctx, a.ID, now); err != nil {
			return Summary{}, err
		}
		a.Declined = Declines{Amounts: []currency.Amount{}}
		if d, ok := declines[a.ID]; ok {
			a.Declined = d
		}
	}

	// The total is taken from every decline, not only the listed agents', so
	// it cannot fall short of what the payments say.
	for _, d := range declines {
		if sum.Declined, err = sum.Declined.add(d); err != nil {
			return Summary{}, err
		}
	}
	return sum, nil
}
