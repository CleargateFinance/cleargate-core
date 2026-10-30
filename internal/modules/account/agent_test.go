//go:build integration

package account

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/crypto"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// agentFixture is an account whose owner can manage agents, which means a
// session that has satisfied a second factor, since issuing a credential is
// gated on one.
type agentFixture struct {
	*memberFixture
}

func newAgentFixture(t *testing.T) *agentFixture {
	t.Helper()
	return &agentFixture{memberFixture: newMemberFixture(t)}
}

// connect registers an agent and returns it.
func (f *agentFixture) connect(t *testing.T, name string) Agent {
	t.Helper()

	agent, err := f.svc.ConnectAgent(context.Background(), f.owner, f.accountID, name)
	require.NoError(t, err)
	return agent
}

// issue mints a credential for an agent.
func (f *agentFixture) issue(t *testing.T, agentID id.AgentID, name string) crypto.APIKey {
	t.Helper()

	key, _, err := f.svc.IssueCredential(context.Background(), f.owner, agentID, name)
	require.NoError(t, err)
	return key
}

// TestIssueCredential_ReturnsSecretOnlyOnce is what makes a leak of the
// credential table worth nothing.
//
// The secret exists in the response and in whatever the caller does with it,
// and nowhere else. Losing it means rotating rather than looking it up, which
// is the property the whole storage decision depends on.
func TestIssueCredential_ReturnsSecretOnlyOnce(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)

	agent := f.connect(t, "invoice-runner")
	key := f.issue(t, agent.ID, "prod-worker-1")

	_, secret, err := crypto.ParseAPIKey(key.Plaintext())
	require.NoError(t, err)

	// The column holds the digest, not the secret.
	var storedDigest []byte
	var storedPrefix string
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT prefix, secret_hash FROM agent_credential WHERE agent_id = $1`,
		string(agent.ID)).Scan(&storedPrefix, &storedDigest))

	assert.Equal(t, key.Prefix(), storedPrefix, "the handle is stored as-is, it is not secret")
	assert.Equal(t, crypto.DigestSecret(secret), storedDigest)
	assert.NotContains(t, string(storedDigest), secret)

	// And nothing that lists credentials afterwards can produce it again.
	credentials, err := f.svc.ListCredentials(ctx, f.owner, agent.ID)
	require.NoError(t, err)
	require.Len(t, credentials, 1)
	assert.Equal(t, key.Prefix(), credentials[0].Prefix)
	assert.NotContains(t, credentials[0].Name, secret)
}

func TestAuthenticateAgent_ResolvesAValidKey(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)

	agent := f.connect(t, "invoice-runner")
	key := f.issue(t, agent.ID, "prod-worker-1")

	identity, err := f.svc.AuthenticateAgent(ctx, key.Plaintext())
	require.NoError(t, err)

	assert.Equal(t, agent.ID, identity.AgentID)
	assert.Equal(t, f.accountID, identity.AccountID)
	assert.Equal(t, key.Prefix(), identity.Prefix)

	// Use is recorded, so a rotation can confirm a key is idle before retiring
	// it.
	credentials, err := f.svc.ListCredentials(ctx, f.owner, agent.ID)
	require.NoError(t, err)
	require.Len(t, credentials, 1)
	assert.NotNil(t, credentials[0].LastUsedAt)
}

// TestVerify_RejectsRevoked covers the narrowest of the three revocation
// widths: one key dies, the agent carries on.
func TestVerify_RejectsRevoked(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)

	agent := f.connect(t, "invoice-runner")
	key := f.issue(t, agent.ID, "prod-worker-1")

	credentials, err := f.svc.ListCredentials(ctx, f.owner, agent.ID)
	require.NoError(t, err)
	require.Len(t, credentials, 1)

	require.NoError(t, f.svc.RevokeCredential(ctx, f.owner, agent.ID, credentials[0].ID))

	_, err = f.svc.AuthenticateAgent(ctx, key.Plaintext())
	assert.ErrorIs(t, err, ErrCredentialInvalid)
}

// TestVerify_RejectsCredentialOfRevokedAgent covers the wider width: revoking
// the agent must take every key it holds with it.
//
// An agent marked revoked whose keys still worked is exactly the state somebody
// containing an incident would believe they had just eliminated.
func TestVerify_RejectsCredentialOfRevokedAgent(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)

	agent := f.connect(t, "invoice-runner")
	first := f.issue(t, agent.ID, "prod-worker-1")
	second := f.issue(t, agent.ID, "prod-worker-2")

	require.NoError(t, f.svc.RevokeAgent(ctx, f.owner, agent.ID))

	for name, key := range map[string]crypto.APIKey{"first": first, "second": second} {
		t.Run(name, func(t *testing.T) {
			_, err := f.svc.AuthenticateAgent(ctx, key.Plaintext())
			assert.ErrorIs(t, err, ErrCredentialInvalid)
		})
	}

	// The rows are marked, not deleted, so the decision history stays
	// attributable to the identity that made it.
	var revoked int
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT count(*) FROM agent_credential WHERE agent_id = $1 AND revoked_at IS NOT NULL`,
		string(agent.ID)).Scan(&revoked))
	assert.Equal(t, 2, revoked)

	// And no new key can be issued against it.
	_, _, err := f.svc.IssueCredential(ctx, f.owner, agent.ID, "prod-worker-3")
	assert.ErrorIs(t, err, ErrAgentRevoked)
}

// TestVerify_ConstantTimeOnMismatch checks the behaviour a constant-time
// comparison is there to produce.
//
// Every way of being wrong gives the same answer: an unknown handle, a known
// handle with the wrong secret, and a secret that differs in its first
// character rather than its last. None of them says how close the guess was, or
// whether the handle existed at all.
//
// The timing itself is deliberately not asserted. A wall-clock test on a
// 32-byte comparison measures scheduling noise, so it would be flaky in CI
// while still passing for an implementation that short-circuited. That
// guarantee comes from subtle.ConstantTimeCompare and is checked by reading the
// code, not by timing it.
func TestVerify_ConstantTimeOnMismatch(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)

	agent := f.connect(t, "invoice-runner")
	key := f.issue(t, agent.ID, "prod-worker-1")

	_, secret, err := crypto.ParseAPIKey(key.Plaintext())
	require.NoError(t, err)

	unknown, err := crypto.NewAPIKey()
	require.NoError(t, err)

	tests := map[string]string{
		"unknown handle":      unknown.Plaintext(),
		"wrong secret":        crypto.KeyEnvironmentLive + "_" + key.Prefix() + "_" + "x" + secret[1:],
		"secret wrong at end": crypto.KeyEnvironmentLive + "_" + key.Prefix() + "_" + secret[:len(secret)-1] + "x",
		"empty secret half":   crypto.KeyEnvironmentLive + "_" + key.Prefix() + "_",
		"garbage":             "not-a-key",
	}

	for name, presented := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := f.svc.AuthenticateAgent(ctx, presented)
			assert.ErrorIs(t, err, ErrCredentialInvalid,
				"every rejection must be the same rejection")
		})
	}
}

// TestAgentCredentialCannotOpenUserSession keeps the two credential systems
// from being interchangeable.
//
// They authenticate different kinds of caller with different powers: a human
// session reaches the console and can create authority, while an agent key can
// only ever request a payment against a mandate. A token accepted by both
// would collapse that separation.
func TestAgentCredentialCannotOpenUserSession(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)

	agent := f.connect(t, "invoice-runner")
	key := f.issue(t, agent.ID, "prod-worker-1")

	// An agent key is not a console session.
	_, err := f.svc.ValidateSession(ctx, key.Plaintext())
	assert.ErrorIs(t, err, ErrSessionInvalid)

	// And a console session is not an agent key.
	sessionToken, _ := f.session(t)
	_, err = f.svc.AuthenticateAgent(ctx, sessionToken)
	assert.ErrorIs(t, err, ErrCredentialInvalid)
}

// TestConnectedAgentWithoutMandateCannotSpend states what connecting an agent
// is worth on its own, which is nothing.
//
// Authentication answers who is calling and says nothing about what they may
// do. Spending authority comes from a mandate, which the mandate module grants
// separately, so the strongest form of this test is that the identity a
// verified key resolves to carries no authority at all.
func TestConnectedAgentWithoutMandateCannotSpend(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)

	agent := f.connect(t, "invoice-runner")
	key := f.issue(t, agent.ID, "prod-worker-1")

	identity, err := f.svc.AuthenticateAgent(ctx, key.Plaintext())
	require.NoError(t, err)

	// Who, and where. Nothing about limits, counterparties or amounts, because
	// none of that is a property of the credential.
	assert.Equal(t, agent.ID, identity.AgentID)
	assert.Equal(t, f.accountID, identity.AccountID)

	// The agent holds no ledger position of its own either. It never holds
	// funds, which is what makes revocation instant: there is a counter to stop,
	// not a balance to unwind.
	var ledgerAccounts int
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT count(*) FROM ledger_account WHERE account_id = $1`,
		string(f.accountID)).Scan(&ledgerAccounts))
	assert.Zero(t, ledgerAccounts,
		"connecting an agent must not create anything that can hold money")
}

// TestRotateCredentialWithoutDowntime is why credentials are separate objects
// from the agent.
//
// Issue the new key, deploy it, revoke the old one. Both work during the
// overlap, so a rotation needs no window where the agent is unable to
// authenticate, and the identity carrying the mandate and the decision history
// is untouched throughout.
func TestRotateCredentialWithoutDowntime(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)

	agent := f.connect(t, "invoice-runner")
	old := f.issue(t, agent.ID, "prod-worker-1")

	// Still serving traffic on the old key.
	_, err := f.svc.AuthenticateAgent(ctx, old.Plaintext())
	require.NoError(t, err)

	fresh := f.issue(t, agent.ID, "prod-worker-2")

	// The overlap: both keys authenticate to the same identity.
	oldIdentity, err := f.svc.AuthenticateAgent(ctx, old.Plaintext())
	require.NoError(t, err)
	newIdentity, err := f.svc.AuthenticateAgent(ctx, fresh.Plaintext())
	require.NoError(t, err)
	assert.Equal(t, oldIdentity.AgentID, newIdentity.AgentID,
		"a rotation must not change the identity that carries the mandate")

	credentials, err := f.svc.ListCredentials(ctx, f.owner, agent.ID)
	require.NoError(t, err)
	require.Len(t, credentials, 2)

	var oldID string
	for _, c := range credentials {
		if c.Prefix == old.Prefix() {
			oldID = c.ID
		}
	}
	require.NotEmpty(t, oldID)

	require.NoError(t, f.svc.RevokeCredential(ctx, f.owner, agent.ID, oldID))

	// The old key is finished and the new one carries on, with no moment in
	// between where neither worked.
	_, err = f.svc.AuthenticateAgent(ctx, old.Plaintext())
	assert.ErrorIs(t, err, ErrCredentialInvalid)

	_, err = f.svc.AuthenticateAgent(ctx, fresh.Plaintext())
	assert.NoError(t, err)
}

// TestIssueCredential_RequiresStepUp is the asymmetry between registering an
// agent and giving it a key.
//
// The registration grants nothing, so it needs no second factor. The key is a
// bearer secret that keeps working long after the session that minted it is
// gone, which is exactly what a stolen cookie should not be able to leave
// behind.
func TestIssueCredential_RequiresStepUp(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)

	f.clock.Advance(DefaultStepUpWindow + time.Minute)

	// Registering still works.
	agent, err := f.svc.ConnectAgent(ctx, f.owner, f.accountID, "late-runner")
	require.NoError(t, err)

	// Issuing does not.
	_, _, err = f.svc.IssueCredential(ctx, f.owner, agent.ID, "prod-worker-1")

	var stepUp *StepUpError
	require.ErrorAs(t, err, &stepUp)
	assert.Equal(t, ReasonStepUpRequired, stepUp.Reason)

	var credentials int
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT count(*) FROM agent_credential`).Scan(&credentials))
	assert.Zero(t, credentials, "a refused issue must not leave a usable key")
}

// TestRevokeNeedsNoStepUp is the other half of that asymmetry. Containing an
// incident must never depend on having a phone to hand.
func TestRevokeNeedsNoStepUp(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)

	agent := f.connect(t, "invoice-runner")
	key := f.issue(t, agent.ID, "prod-worker-1")

	f.clock.Advance(DefaultStepUpWindow + time.Minute)

	require.NoError(t, f.svc.RevokeAgent(ctx, f.owner, agent.ID))

	_, err := f.svc.AuthenticateAgent(ctx, key.Plaintext())
	assert.ErrorIs(t, err, ErrCredentialInvalid)
}

// TestAgentManagement_RequiresTheRightRole keeps agents inside the roles that
// are supposed to manage them.
func TestAgentManagement_RequiresTheRightRole(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)

	agent := f.connect(t, "invoice-runner")

	// A viewer reads the console and changes nothing.
	viewer := f.joinAsNewUser(t, f.invite(t, f.owner, "viewer@example.com", RoleViewer))
	viewerToken, _, err := f.svc.IssueSession(ctx, NewSessionParams{UserID: viewer})
	require.NoError(t, err)
	viewerSession, err := f.svc.ValidateSession(ctx, viewerToken.Plaintext())
	require.NoError(t, err)

	_, err = f.svc.ConnectAgent(ctx, viewerSession, f.accountID, "rogue")
	assert.ErrorIs(t, err, ErrNotPermitted)

	_, _, err = f.svc.IssueCredential(ctx, viewerSession, agent.ID, "rogue-key")
	assert.ErrorIs(t, err, ErrNotPermitted)

	assert.ErrorIs(t, f.svc.RevokeAgent(ctx, viewerSession, agent.ID), ErrNotPermitted)

	// Somebody with no membership here at all gets the same answer.
	strangerToken := f.signInStranger(t, "outsider@example.com")
	strangerSession, err := f.svc.ValidateSession(ctx, strangerToken)
	require.NoError(t, err)

	_, err = f.svc.ConnectAgent(ctx, strangerSession, f.accountID, "rogue")
	assert.ErrorIs(t, err, ErrNotPermitted)
}

// TestStartAgentSession_GroupsARun covers the record that cannot be
// reconstructed afterwards.
func TestStartAgentSession_GroupsARun(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)

	agent := f.connect(t, "invoice-runner")
	key := f.issue(t, agent.ID, "prod-worker-1")

	identity, err := f.svc.AuthenticateAgent(ctx, key.Plaintext())
	require.NoError(t, err)

	run, err := f.svc.StartAgentSession(ctx, identity, StartAgentSessionParams{
		TaskRef:  "job-4711",
		TaskText: "pay the outstanding invoices for September",
	})
	require.NoError(t, err)

	assert.NotEmpty(t, run.ID)
	assert.Equal(t, agent.ID, run.AgentID)
	assert.False(t, run.StartedAt.IsZero())

	// The identity comes from the verified credential, so a run cannot be
	// opened against somebody else's agent.
	var storedAgent string
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT agent_id FROM agent_session WHERE id = $1`, string(run.ID)).Scan(&storedAgent))
	assert.Equal(t, string(agent.ID), storedAgent)
}

// TestRequireMandateIssuer_AllowsManagersWithStepUp: the roles that manage
// agents grant them authority, once they have entered a second factor.
func TestRequireMandateIssuer_AllowsManagersWithStepUp(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)

	assert.NoError(t, f.svc.RequireMandateIssuer(ctx, f.owner, f.accountID))

	admin := f.addMemberWithMFA(t, "admin@example.com", RoleAdmin)
	assert.NoError(t, f.svc.RequireMandateIssuer(ctx, admin, f.accountID))
}

// TestRequireMandateIssuer_RefusesOtherRoles: a guardian's power is
// subtractive and a viewer changes nothing, so neither may grant spending
// authority, second factor or not.
func TestRequireMandateIssuer_RefusesOtherRoles(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)

	guardian := f.addMemberWithMFA(t, "guardian@example.com", RoleGuardian)
	assert.ErrorIs(t, f.svc.RequireMandateIssuer(ctx, guardian, f.accountID), ErrNotPermitted)

	viewer := f.addMemberWithMFA(t, "viewer@example.com", RoleViewer)
	assert.ErrorIs(t, f.svc.RequireMandateIssuer(ctx, viewer, f.accountID), ErrNotPermitted)

	stranger, err := f.svc.ValidateSession(ctx, f.signInStranger(t, "stranger@example.com"))
	require.NoError(t, err)
	assert.ErrorIs(t, f.svc.RequireMandateIssuer(ctx, stranger, f.accountID), ErrNotPermitted)
}

// TestRequireMandateIssuer_RequiresRecentStepUp: until passkey signing ships,
// the step-up is the whole of a mandate's approval, so a stale one is refused.
func TestRequireMandateIssuer_RequiresRecentStepUp(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t)

	f.clock.Advance(DefaultStepUpWindow + time.Minute)

	err := f.svc.RequireMandateIssuer(ctx, f.owner, f.accountID)
	var stepUp *StepUpError
	require.ErrorAs(t, err, &stepUp)
	assert.Equal(t, ReasonStepUpRequired, stepUp.Reason)
}
