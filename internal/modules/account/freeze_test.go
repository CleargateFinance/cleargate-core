//go:build integration

package account

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

// TestFreezeAndSuspensionAreIndependent: a customer's freeze and a platform
// suspension must not share state, or unfreezing would lift a suspension.
func TestFreezeAndSuspensionAreIndependent(t *testing.T) {
	ctx := context.Background()
	db := fixtures.NewTestDB(t)

	var accountID string
	require.NoError(t, db.QueryRow(ctx, `
		WITH p AS (INSERT INTO principal (kind) VALUES ('person') RETURNING id)
		INSERT INTO account (principal_id, display_name)
		SELECT id, 'test' FROM p
		RETURNING id`).Scan(&accountID))

	_, err := db.Exec(ctx, `UPDATE account SET frozen_at = now() WHERE id = $1`, accountID)
	require.NoError(t, err, "an active account can be frozen")

	_, err = db.Exec(ctx, `UPDATE account SET status = 'suspended', frozen_at = NULL WHERE id = $1`, accountID)
	require.NoError(t, err, "a suspended account can be unfrozen")
}

// TestSpendingBlocked_UnlessActiveAndUnfrozen: either a freeze or a
// suspension stops spending, and only an active, unfrozen account may spend.
func TestSpendingBlocked_UnlessActiveAndUnfrozen(t *testing.T) {
	ctx := context.Background()
	db := fixtures.NewTestDB(t)
	repo := NewPostgresRepository(db.DB)

	var accountID string
	require.NoError(t, db.QueryRow(ctx, `
		WITH p AS (INSERT INTO principal (kind) VALUES ('person') RETURNING id)
		INSERT INTO account (principal_id, display_name)
		SELECT id, 'test' FROM p
		RETURNING id`).Scan(&accountID))

	states := []struct {
		name    string
		update  string
		blocked bool
	}{
		{"active", `UPDATE account SET status = 'active', frozen_at = NULL WHERE id = $1`, false},
		{"frozen", `UPDATE account SET frozen_at = now() WHERE id = $1`, true},
		{"suspended", `UPDATE account SET status = 'suspended', frozen_at = NULL WHERE id = $1`, true},
	}
	for _, s := range states {
		_, err := db.Exec(ctx, s.update, accountID)
		require.NoError(t, err)

		blocked, err := repo.SpendingBlocked(ctx, id.AccountID(accountID))
		require.NoError(t, err)
		require.Equal(t, s.blocked, blocked, s.name)
	}

	_, err := repo.SpendingBlocked(ctx, id.AccountID("00000000-0000-0000-0000-000000000000"))
	require.ErrorIs(t, err, ErrAccountNotFound)
}

// TestFreezeAccount_StopsSpendingUntilUnfrozen: the account-wide revocation,
// and the way back from it.
func TestFreezeAccount_StopsSpendingUntilUnfrozen(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)
	agent := f.connect(t, "invoice-runner")

	require.NoError(t, f.svc.FreezeAccount(ctx, f.owner, f.accountID))

	blocked, err := f.svc.SpendingBlocked(ctx, f.accountID)
	require.NoError(t, err)
	assert.True(t, blocked)
	allowed, err := f.svc.HoldSpending(ctx, f.accountID, agent.ID)
	require.NoError(t, err)
	assert.False(t, allowed, "the check inside the payment's transaction sees it too")

	accounts, err := f.svc.Accounts(ctx, f.owner)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	assert.NotNil(t, accounts[0].FrozenAt)

	require.NoError(t, f.svc.UnfreezeAccount(ctx, f.owner, f.accountID))

	blocked, err = f.svc.SpendingBlocked(ctx, f.accountID)
	require.NoError(t, err)
	assert.False(t, blocked)
	var frozenBy *string
	require.NoError(t, f.db.QueryRow(ctx, `SELECT frozen_by FROM account WHERE id = $1`,
		string(f.accountID)).Scan(&frozenBy))
	assert.Nil(t, frozenBy)
}

// TestFreezeAccount_KeepsWhoFrozeItFirst: a second freeze is not an error and
// does not rewrite the record of who acted.
func TestFreezeAccount_KeepsWhoFrozeItFirst(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)
	admin := f.addMemberWithMFA(t, "admin@example.com", RoleAdmin)

	require.NoError(t, f.svc.FreezeAccount(ctx, f.owner, f.accountID))
	first := f.frozen(t)
	f.clock.Advance(time.Minute)
	require.NoError(t, f.svc.FreezeAccount(ctx, admin, f.accountID))

	assert.Equal(t, first, f.frozen(t))
	assert.Equal(t, string(f.owner.UserID), first.by)
}

// TestUnfreezeAccount_RequiresAStepUp: a stolen session alone must not undo a
// freeze, which is usually there because somebody suspects one.
func TestUnfreezeAccount_RequiresAStepUp(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)
	require.NoError(t, f.svc.FreezeAccount(ctx, f.owner, f.accountID))

	f.clock.Advance(DefaultStepUpWindow + time.Minute)
	err := f.svc.UnfreezeAccount(ctx, f.owner, f.accountID)
	var stepUp *StepUpError
	require.ErrorAs(t, err, &stepUp)
	assert.Equal(t, ReasonStepUpRequired, stepUp.Reason)

	assert.NoError(t, f.svc.FreezeAccount(ctx, f.owner, f.accountID),
		"freezing needs no step-up")
	blocked, err := f.svc.SpendingBlocked(ctx, f.accountID)
	require.NoError(t, err)
	assert.True(t, blocked)
}

// TestUnfreezeAccount_LeavesASuspensionAlone: the customer lifts their own
// freeze and nothing the platform imposed.
func TestUnfreezeAccount_LeavesASuspensionAlone(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)
	require.NoError(t, f.svc.FreezeAccount(ctx, f.owner, f.accountID))
	_, err := f.db.Exec(ctx, `UPDATE account SET status = 'suspended' WHERE id = $1`, string(f.accountID))
	require.NoError(t, err)

	require.NoError(t, f.svc.UnfreezeAccount(ctx, f.owner, f.accountID))

	blocked, err := f.svc.SpendingBlocked(ctx, f.accountID)
	require.NoError(t, err)
	assert.True(t, blocked)
}

// TestFreeze_Roles: a guardian may freeze, since freezing only subtracts, but
// may not unfreeze; a viewer changes nothing; an outsider gets what a viewer
// gets.
func TestFreeze_Roles(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)
	guardian := f.addMemberWithMFA(t, "guardian@example.com", RoleGuardian)
	viewer := f.addMemberWithMFA(t, "viewer@example.com", RoleViewer)
	stranger, err := f.svc.ValidateSession(ctx, f.signInStranger(t, "stranger@example.com"))
	require.NoError(t, err)

	assert.ErrorIs(t, f.svc.FreezeAccount(ctx, viewer, f.accountID), ErrNotPermitted)
	assert.ErrorIs(t, f.svc.FreezeAccount(ctx, stranger, f.accountID), ErrNotPermitted)
	assert.Nil(t, f.frozen(t).at, "a refused freeze leaves the account as it was")

	require.NoError(t, f.svc.FreezeAccount(ctx, guardian, f.accountID))
	assert.ErrorIs(t, f.svc.UnfreezeAccount(ctx, guardian, f.accountID), ErrNotPermitted)
	assert.ErrorIs(t, f.svc.UnfreezeAccount(ctx, viewer, f.accountID), ErrNotPermitted)
	assert.ErrorIs(t, f.svc.UnfreezeAccount(ctx, stranger, f.accountID), ErrNotPermitted)
	assert.NotNil(t, f.frozen(t).at)
}

type freezeRecord struct {
	at *time.Time
	by string
}

func (f *agentFixture) frozen(t *testing.T) freezeRecord {
	t.Helper()
	var r freezeRecord
	var by *string
	require.NoError(t, f.db.QueryRow(context.Background(),
		`SELECT frozen_at, frozen_by FROM account WHERE id = $1`, string(f.accountID)).Scan(&r.at, &by))
	if by != nil {
		r.by = *by
	}
	return r
}

// TestAccountsAndAgents_ListWhatTheMemberCanSee: the console's starting
// point. Each member sees their own role, and any member may list agents.
func TestAccountsAndAgents_ListWhatTheMemberCanSee(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)
	first := f.connect(t, "invoice-runner")
	second := f.connect(t, "retired")
	require.NoError(t, f.svc.RevokeAgent(ctx, f.owner, second.ID))
	viewer := f.addMemberWithMFA(t, "viewer@example.com", RoleViewer)

	accounts, err := f.svc.Accounts(ctx, viewer)
	require.NoError(t, err)
	require.Len(t, accounts, 1, "an invited member has no account of their own")
	assert.Equal(t, f.accountID, accounts[0].AccountID)
	assert.Equal(t, RoleViewer, accounts[0].Role)
	assert.NotEmpty(t, accounts[0].Name)
	assert.Nil(t, accounts[0].FrozenAt)

	agents, err := f.svc.Agents(ctx, viewer, f.accountID)
	require.NoError(t, err)
	require.Len(t, agents, 2)
	assert.Equal(t, first.ID, agents[0].ID)
	assert.True(t, agents[0].Active())
	assert.Equal(t, second.ID, agents[1].ID)
	assert.False(t, agents[1].Active(), "a revoked agent stays listed with its history")

	stranger, err := f.svc.ValidateSession(ctx, f.signInStranger(t, "stranger@example.com"))
	require.NoError(t, err)
	_, err = f.svc.Agents(ctx, stranger, f.accountID)
	assert.ErrorIs(t, err, ErrNotPermitted)

	theirs, err := f.svc.Accounts(ctx, stranger)
	require.NoError(t, err)
	require.Len(t, theirs, 1)
	assert.Equal(t, RoleOwner, theirs[0].Role)
	assert.NotEqual(t, f.accountID, theirs[0].AccountID)

	none, err := f.svc.Agents(ctx, stranger, theirs[0].AccountID)
	require.NoError(t, err)
	assert.NotNil(t, none, "an account with no agents lists [], not null")
	assert.Empty(t, none)
}
