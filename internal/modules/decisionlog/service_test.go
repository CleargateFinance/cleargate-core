package decisionlog

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// fakeRepo serves entries newest first, ignoring the cursor; the database
// test covers paging itself.
type fakeRepo struct {
	Repository
	entries []Entry
	asked   *Query
}

func (r *fakeRepo) AgentDecisions(_ context.Context, _ id.AgentID, q Query) ([]Entry, error) {
	r.asked = &q
	if q.Limit < len(r.entries) {
		return r.entries[:q.Limit], nil
	}
	return r.entries, nil
}

type fakeAccounts struct{ err error }

func (a fakeAccounts) RequireAgentReader(context.Context, Actor, id.AgentID) error { return a.err }

type fakePayments map[id.PaymentID]Payment

func (p fakePayments) AgentPayments(_ context.Context, _ id.AgentID, ids []id.PaymentID) (map[id.PaymentID]Payment, error) {
	out := map[id.PaymentID]Payment{}
	for _, i := range ids {
		if pay, ok := p[i]; ok {
			out[i] = pay
		}
	}
	return out, nil
}

// logOf builds n entries, newest first, each with its payment.
func logOf(t *testing.T, n int) (*fakeRepo, fakePayments) {
	repo, payments := &fakeRepo{}, fakePayments{}
	start := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	for i := range n {
		paymentID := id.PaymentID(fmt.Sprintf("p%d", i))
		repo.entries = append(repo.entries, Entry{
			ID: id.DecisionID(fmt.Sprintf("d%d", i)), PaymentID: paymentID,
			EvaluatedAt: start.Add(-time.Duration(i) * time.Minute),
		})
		amount, err := currency.Parse(fmt.Sprintf("%d.50", i), currency.USD)
		require.NoError(t, err)
		payments[paymentID] = Payment{Amount: amount, Status: "declined"}
	}
	return repo, payments
}

func TestLog_PagesWithACursorToTheLastEntryShown(t *testing.T) {
	repo, payments := logOf(t, 5)
	log := NewLog(repo, fakeAccounts{}, payments)

	page, err := log.AgentDecisions(context.Background(), Actor{UserID: "u"}, "agent", Query{Limit: 3})

	require.NoError(t, err)
	assert.Equal(t, 4, repo.asked.Limit, "one more than the page, to know whether there is another")
	require.Len(t, page.Entries, 3)
	require.NotNil(t, page.Next)
	assert.Equal(t, Cursor{EvaluatedAt: page.Entries[2].EvaluatedAt, ID: "d2"}, *page.Next)
	assert.Equal(t, "2.5", page.Entries[2].Payment.Amount.String(), "each entry carries its own payment")
}

func TestLog_TheLastPageHasNoCursor(t *testing.T) {
	repo, payments := logOf(t, 3)
	log := NewLog(repo, fakeAccounts{}, payments)

	page, err := log.AgentDecisions(context.Background(), Actor{UserID: "u"}, "agent", Query{Limit: 3})

	require.NoError(t, err)
	assert.Len(t, page.Entries, 3)
	assert.Nil(t, page.Next, "a page that fits exactly must not lead to an empty one")
}

func TestLog_DefaultsThePageSize(t *testing.T) {
	repo, payments := logOf(t, 0)

	_, err := NewLog(repo, fakeAccounts{}, payments).AgentDecisions(context.Background(), Actor{}, "agent", Query{})

	require.NoError(t, err)
	assert.Equal(t, DefaultPageSize+1, repo.asked.Limit)
}

func TestLog_RefusesBeforeReading(t *testing.T) {
	repo, payments := logOf(t, 2)
	notFound := apperr.NotFound("agent not found")

	_, err := NewLog(repo, fakeAccounts{err: notFound}, payments).
		AgentDecisions(context.Background(), Actor{UserID: "stranger"}, "agent", Query{})

	assert.ErrorIs(t, err, notFound)
	assert.Nil(t, repo.asked, "nothing is read for somebody who may not see it")
}

func TestLog_ADecisionWithoutItsPaymentIsADefect(t *testing.T) {
	repo, payments := logOf(t, 2)
	delete(payments, "p1")

	_, err := NewLog(repo, fakeAccounts{}, payments).AgentDecisions(context.Background(), Actor{}, "agent", Query{})

	require.Error(t, err)
	assert.Equal(t, apperr.KindInternal, apperr.KindOf(err), "not shown as a blank row")
}

func TestNew_RefusesAMissingPort(t *testing.T) {
	_, err := New(Deps{})
	assert.ErrorIs(t, err, ErrMissingDependency)
}
