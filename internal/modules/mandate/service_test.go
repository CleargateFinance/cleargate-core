package mandate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// These tests cover the service's own decisions against fakes of its ports.
// What the database guarantees is tested against a real one, in
// repo_postgres_test.go.

type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time { return c.now }

type fakeAccounts struct {
	agent    Agent
	agentErr error
	issueErr error

	mu    sync.Mutex
	asked []id.AccountID
}

func (f *fakeAccounts) Agent(context.Context, id.AgentID) (Agent, error) {
	return f.agent, f.agentErr
}

func (f *fakeAccounts) RequireMandateIssuer(_ context.Context, _ Actor, accountID id.AccountID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, accountID)
	return f.issueErr
}

// fakeRepo stores issued mandates in memory, enough to see whether the
// service wrote anything and what it would have read back.
type fakeRepo struct {
	issued []Draft
	active *Mandate
}

func (f *fakeRepo) IssueVersion(_ context.Context, _ id.AgentID, _ time.Time, build func(int) (Draft, error)) (Mandate, error) {
	d, err := build(len(f.issued) + 1)
	if err != nil {
		return Mandate{}, err
	}
	f.issued = append(f.issued, d)
	return Mandate{
		ID: "m", AgentID: d.AgentID, Version: d.Version, Payload: d.Payload,
		Scheme: d.Scheme, SignedBy: d.SignedBy, Status: d.Status, EffectiveFrom: d.EffectiveFrom,
	}, nil
}

func (f *fakeRepo) Active(context.Context, id.AgentID) (Mandate, error) {
	if f.active == nil {
		return Mandate{}, ErrNoMandate
	}
	return *f.active, nil
}

func (f *fakeRepo) ActiveAt(ctx context.Context, agentID id.AgentID, _ time.Time) (Mandate, error) {
	return f.Active(ctx, agentID)
}

func (f *fakeRepo) HoldActive(context.Context, id.AgentID) (id.MandateID, error) {
	if f.active == nil {
		return "", nil
	}
	return f.active.ID, nil
}

const (
	testAgent   = id.AgentID("6f1c3a52-6a3e-4c55-9a51-1c0d7a1b2e33")
	testAccount = id.AccountID("0b8e2f4c-1d2a-4b7e-9c3f-5a6d7e8f9a0b")
)

func newTestService(repo *fakeRepo, accounts *fakeAccounts) *Service {
	return NewService(repo, accounts, &fixedClock{now: testNow}, nil)
}

func activeAgent() *fakeAccounts {
	return &fakeAccounts{agent: Agent{ID: testAgent, AccountID: testAccount, Active: true}}
}

func TestIssue_StoresStepUpMandateInForceNow(t *testing.T) {
	repo := &fakeRepo{}
	svc := newTestService(repo, activeAgent())

	m, err := svc.Issue(context.Background(), Actor{UserID: "u1"}, testAgent, validRules(t))
	require.NoError(t, err)

	require.Len(t, repo.issued, 1)
	d := repo.issued[0]
	assert.Equal(t, SchemeSessionStepUp, d.Scheme)
	assert.Equal(t, StatusActive, d.Status, "no timelock yet, so a mandate is in force at once")
	assert.Equal(t, testNow, d.EffectiveFrom)
	assert.Equal(t, id.UserID("u1"), d.SignedBy)

	// What comes back was parsed from the stored bytes.
	assert.Equal(t, []string{"data_api", "inference"}, m.Rules.Categories)
	assert.True(t, m.Rules.MaxPerTx.Equal(amount(t, "20")))
}

// TestIssue_RequiresRecentStepUp: when the account module refuses the actor,
// nothing is stored. Which refusals it gives is tested in the account module.
func TestIssue_RequiresRecentStepUp(t *testing.T) {
	repo := &fakeRepo{}
	accounts := activeAgent()
	accounts.issueErr = apperr.Forbidden("this action needs your second factor re-entered")
	svc := newTestService(repo, accounts)

	_, err := svc.Issue(context.Background(), Actor{UserID: "u1"}, testAgent, validRules(t))
	require.ErrorIs(t, err, accounts.issueErr)
	assert.Empty(t, repo.issued)

	// And it was asked about the agent's own account, not one the caller named.
	assert.Equal(t, []id.AccountID{testAccount}, accounts.asked)
}

func TestIssue_RefusesRevokedAgent(t *testing.T) {
	repo := &fakeRepo{}
	accounts := activeAgent()
	accounts.agent.Active = false
	svc := newTestService(repo, accounts)

	_, err := svc.Issue(context.Background(), Actor{UserID: "u1"}, testAgent, validRules(t))
	require.ErrorIs(t, err, ErrAgentRevoked)
	assert.Empty(t, repo.issued)
	assert.Empty(t, accounts.asked)
}

func TestIssue_RefusesInvalidRulesBeforeAskingAnyone(t *testing.T) {
	repo := &fakeRepo{}
	accounts := activeAgent()
	svc := newTestService(repo, accounts)

	r := validRules(t)
	r.Categories = nil
	_, err := svc.Issue(context.Background(), Actor{UserID: "u1"}, testAgent, r)
	assert.Equal(t, apperr.KindInvalid, apperr.KindOf(err))
	assert.Empty(t, repo.issued)
	assert.Empty(t, accounts.asked)
}

func TestIssue_PassesThroughUnknownAgent(t *testing.T) {
	accounts := activeAgent()
	accounts.agentErr = apperr.NotFound("agent not found")
	svc := newTestService(&fakeRepo{}, accounts)

	_, err := svc.Issue(context.Background(), Actor{UserID: "u1"}, testAgent, validRules(t))
	assert.Equal(t, apperr.KindNotFound, apperr.KindOf(err))
}

// TestCurrent_RefusesUnverifiableScheme is the MVP's form of
// TestAuthorizationRejectsMandateWithInvalidSignature. No WebAuthn verifier
// exists yet, so a mandate claiming that approval is refused rather than
// trusted for sounding strong.
func TestCurrent_RefusesUnverifiableScheme(t *testing.T) {
	payload, err := Canonicalise(testAgent, 1, validRules(t))
	require.NoError(t, err)

	repo := &fakeRepo{active: &Mandate{
		ID: "m", AgentID: testAgent, Version: 1, Payload: payload, Scheme: SchemeWebAuthn,
	}}
	svc := newTestService(repo, activeAgent())

	_, err = svc.Current(context.Background(), testAgent)
	assert.ErrorIs(t, err, ErrUnverifiable)
}

func TestCurrent_RefusesPayloadForAnotherRow(t *testing.T) {
	payload, err := Canonicalise("another-agent", 1, validRules(t))
	require.NoError(t, err)

	repo := &fakeRepo{active: &Mandate{
		ID: "m", AgentID: testAgent, Version: 1, Payload: payload, Scheme: SchemeSessionStepUp,
	}}
	svc := newTestService(repo, activeAgent())

	_, err = svc.Current(context.Background(), testAgent)
	require.Error(t, err)
	assert.Equal(t, apperr.KindInternal, apperr.KindOf(err))
}

func TestCurrent_ReportsNoMandate(t *testing.T) {
	svc := newTestService(&fakeRepo{}, activeAgent())

	_, err := svc.Current(context.Background(), testAgent)
	assert.True(t, errors.Is(err, ErrNoMandate))
}

// TestHoldCurrent_IsFalseOnceAnotherVersionIsInForce: the payment path asks
// whether the mandate it decided against is still the one in force.
func TestHoldCurrent_IsFalseOnceAnotherVersionIsInForce(t *testing.T) {
	repo := &fakeRepo{active: &Mandate{ID: "m2"}}
	svc := newTestService(repo, activeAgent())

	still, err := svc.HoldCurrent(context.Background(), testAgent, "m2")
	require.NoError(t, err)
	assert.True(t, still)

	still, err = svc.HoldCurrent(context.Background(), testAgent, "m1")
	require.NoError(t, err)
	assert.False(t, still)

	repo.active = nil
	still, err = svc.HoldCurrent(context.Background(), testAgent, "m2")
	require.NoError(t, err)
	assert.False(t, still, "no mandate in force is not the one decided against")
}

func TestNew_RefusesMissingAccounts(t *testing.T) {
	_, err := New(Deps{})
	assert.ErrorIs(t, err, ErrMissingDependency)
}
