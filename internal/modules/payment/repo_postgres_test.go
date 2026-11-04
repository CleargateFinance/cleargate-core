//go:build integration

package payment

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// TestLookup_ReturnsOnlyTheAgentsOwnPayments: the decision log asks for a
// page's payments by id, and an id belonging to another agent must not come
// back even if it is asked for.
func TestLookup_ReturnsOnlyTheAgentsOwnPayments(t *testing.T) {
	ctx := context.Background()
	f := newSchemaFixture(t)

	approvedID, err := f.record(f.approved())
	require.NoError(t, err)
	p, d := f.declinedNoMandate()
	p.amount, d.reason = "-5", "invalid_amount"
	declinedID, err := f.record(p, d)
	require.NoError(t, err)

	p, d = f.declinedNoMandate()
	p.accountID, p.agentID = f.otherAccountID, f.otherAgentID
	othersID, err := f.record(p, d)
	require.NoError(t, err)

	got, err := NewLookup(f.db.DB).AgentPayments(ctx, id.AgentID(f.agentID),
		[]id.PaymentID{id.PaymentID(approvedID), id.PaymentID(declinedID), id.PaymentID(othersID)})
	require.NoError(t, err)

	byID := map[id.PaymentID]Recorded{}
	for _, r := range got {
		byID[r.ID] = r
	}
	require.Len(t, byID, 2, "another agent's payment is left out")

	approved := byID[id.PaymentID(approvedID)]
	assert.Equal(t, "0.4", approved.Amount.String())
	assert.Equal(t, StatusAuthorized, approved.Status)
	assert.Equal(t, "data_api", approved.Category)
	assert.Equal(t, "payee-address", approved.PayTo)
	assert.Empty(t, approved.ResourceURL, "not sent reads as empty")

	declined := byID[id.PaymentID(declinedID)]
	assert.Equal(t, "-5", declined.Amount.String(), "the amount as requested, even when invalid")
	assert.Equal(t, StatusDeclined, declined.Status)
}

func TestLookup_NoIdsReadsNothing(t *testing.T) {
	got, err := (&Lookup{}).AgentPayments(context.Background(), "agent", nil)

	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestLookup_DeclinesSinceCountsEveryDeclineAndSumsWhatCouldHaveMoved: the
// console's blocked count. An approval is not a decline, another account's
// declines are not ours, and an invalid amount is counted but adds nothing.
func TestLookup_DeclinesSinceCountsEveryDeclineAndSumsWhatCouldHaveMoved(t *testing.T) {
	ctx := context.Background()
	f := newSchemaFixture(t)

	_, err := f.record(f.approved())
	require.NoError(t, err)
	for _, amount := range []string{"0.40", "12.5", "-5"} {
		p, d := f.declinedNoMandate()
		p.amount = amount
		if amount == "-5" {
			d.reason = "invalid_amount"
		}
		_, err = f.record(p, d)
		require.NoError(t, err)
	}
	p, d := f.declinedNoMandate()
	p.accountID, p.agentID = f.otherAccountID, f.otherAgentID
	_, err = f.record(p, d)
	require.NoError(t, err)

	lookup := NewLookup(f.db.DB)
	got, err := lookup.DeclinesSince(ctx, id.AccountID(f.accountID), f.at)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, id.AgentID(f.agentID), got[0].AgentID)
	assert.Equal(t, 3, got[0].Count)
	assert.Equal(t, "12.9", got[0].Amount.String())
	assert.Equal(t, "USD", string(got[0].Amount.Asset()))

	later, err := lookup.DeclinesSince(ctx, id.AccountID(f.accountID), f.at.Add(time.Microsecond))
	require.NoError(t, err)
	assert.Empty(t, later, "a decline before the window is not in it")
}
