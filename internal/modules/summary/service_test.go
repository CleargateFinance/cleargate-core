package summary

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

func amount(t *testing.T, s string, a currency.Asset) currency.Amount {
	t.Helper()
	v, err := currency.Parse(s, a)
	require.NoError(t, err)
	return v
}

type fakeAccounts struct {
	err    error
	agents []Agent
}

func (f fakeAccounts) Membership(_ context.Context, _ Actor, accountID id.AccountID) (Account, error) {
	return Account{ID: accountID, Name: "acme", Role: "viewer"}, f.err
}

func (f fakeAccounts) Agents(context.Context, Actor, id.AccountID) ([]Agent, error) {
	return f.agents, f.err
}

// fakeLedger records what it was asked, so a test can tell whether a refused
// reader caused any read at all.
type fakeLedger struct {
	t     *testing.T
	spent map[id.AgentID][2]string
	at    time.Time
	reads int
}

func (f *fakeLedger) Balance(context.Context, id.AccountID) (currency.Amount, error) {
	f.reads++
	return amount(f.t, "70", currency.USD), nil
}

func (f *fakeLedger) Spent(_ context.Context, agentID id.AgentID, at time.Time) (currency.Amount, currency.Amount, error) {
	f.reads++
	f.at = at
	s := f.spent[agentID]
	return amount(f.t, s[0], currency.USD), amount(f.t, s[1], currency.USD), nil
}

func (f *fakeLedger) Windows(at time.Time) (time.Time, time.Time) {
	return at.Truncate(24 * time.Hour), time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
}

type fakeMandates map[id.AgentID]*Caps

func (f fakeMandates) Current(_ context.Context, agentID id.AgentID) (*Caps, error) {
	return f[agentID], nil
}

type fakePayments struct {
	declines map[id.AgentID]Declines
	since    time.Time
}

func (f *fakePayments) DeclinesSince(_ context.Context, _ id.AccountID, since time.Time) (map[id.AgentID]Declines, error) {
	f.since = since
	return f.declines, nil
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func TestAccount_EachAgentAgainstItsCapsAndTheDeclinesAddedUp(t *testing.T) {
	now := time.Date(2026, 11, 4, 15, 30, 0, 0, time.UTC)
	caps := &Caps{ID: "m1", Version: 2, MaxPerTx: amount(t, "20", currency.USD)}
	ledger := &fakeLedger{t: t, spent: map[id.AgentID][2]string{"buyer": {"15", "40"}, "idle": {"0", "0"}}}
	payments := &fakePayments{declines: map[id.AgentID]Declines{
		"buyer": {Count: 3, Amounts: []currency.Amount{amount(t, "25", currency.USD)}},
		"idle":  {Count: 2, Amounts: []currency.Amount{amount(t, "5", currency.USD), amount(t, "1", "EURC")}},
	}}
	svc := NewService(
		fakeAccounts{agents: []Agent{{ID: "buyer", Name: "buyer"}, {ID: "idle", Name: "idle"}}},
		ledger, fakeMandates{"buyer": caps}, payments, fixedClock{now})

	sum, err := svc.Account(context.Background(), Actor{UserID: "u"}, "acct")

	require.NoError(t, err)
	assert.Equal(t, "70", sum.Balance.String())
	assert.Equal(t, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), sum.MonthStart)
	assert.Equal(t, sum.MonthStart, payments.since, "declines are counted from the start of the month")
	assert.Equal(t, now, ledger.at, "spending is read in the windows containing now")

	require.Len(t, sum.Agents, 2)
	buyer, idle := sum.Agents[0], sum.Agents[1]
	assert.Same(t, caps, buyer.Mandate)
	assert.Equal(t, "15", buyer.SpentToday.String())
	assert.Equal(t, "40", buyer.SpentThisMonth.String())
	assert.Equal(t, 3, buyer.Declined.Count)
	assert.Nil(t, idle.Mandate, "an agent with no mandate has no caps to show")

	assert.Equal(t, 5, sum.Declined.Count)
	require.Len(t, sum.Declined.Amounts, 2, "one total per asset, never added across them")
	assert.Equal(t, "1 EURC", sum.Declined.Amounts[0].String()+" "+string(sum.Declined.Amounts[0].Asset()))
	assert.Equal(t, "30 USD", sum.Declined.Amounts[1].String()+" "+string(sum.Declined.Amounts[1].Asset()))
}

func TestAccount_AnAgentWithNoDeclinesShowsNone(t *testing.T) {
	svc := NewService(fakeAccounts{agents: []Agent{{ID: "buyer"}}},
		&fakeLedger{t: t, spent: map[id.AgentID][2]string{"buyer": {"0", "0"}}},
		fakeMandates{}, &fakePayments{}, fixedClock{time.Now()})

	sum, err := svc.Account(context.Background(), Actor{}, "acct")

	require.NoError(t, err)
	assert.NotNil(t, sum.Agents[0].Declined.Amounts, "an empty list, not a missing one")
	assert.NotNil(t, sum.Declined.Amounts)
	assert.Zero(t, sum.Declined.Count)
}

func TestAccount_RefusesBeforeReading(t *testing.T) {
	notPermitted := apperr.Forbidden("your role does not permit this change")
	ledger := &fakeLedger{t: t}
	svc := NewService(fakeAccounts{err: notPermitted}, ledger, fakeMandates{}, &fakePayments{}, fixedClock{time.Now()})

	_, err := svc.Account(context.Background(), Actor{UserID: "stranger"}, "acct")

	assert.ErrorIs(t, err, notPermitted)
	assert.Zero(t, ledger.reads, "nothing is read for somebody who may not see it")
}

func TestDeclines_AddKeepsOneTotalPerAsset(t *testing.T) {
	a := Declines{Count: 1, Amounts: []currency.Amount{amount(t, "2", currency.USD)}}
	b := Declines{Count: 2, Amounts: []currency.Amount{amount(t, "3", currency.USD), amount(t, "4", "EURC")}}

	got, err := a.add(b)

	require.NoError(t, err)
	assert.Equal(t, 3, got.Count)
	require.Len(t, got.Amounts, 2)
	assert.Equal(t, "5", got.Amounts[1].String())
	assert.Equal(t, "2", a.Amounts[0].String(), "adding leaves the original alone")
}

func TestNew_RefusesAMissingPort(t *testing.T) {
	_, err := New(Deps{})
	assert.ErrorIs(t, err, ErrMissingDependency)

	_, err = New(Deps{Accounts: fakeAccounts{}, Ledger: &fakeLedger{}, Mandates: fakeMandates{}})
	assert.ErrorIs(t, err, ErrMissingDependency, "payments")
}
