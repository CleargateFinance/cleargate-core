//go:build integration

package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/modules/account"
)

// These tests drive Task 6.2's agent and freeze routes: a person who signed up
// over HTTP finds their account, connects an agent, gives it a key, sets its
// mandate, and can switch all of it off again.

type accountJSON struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Role     string     `json:"role"`
	FrozenAt *time.Time `json:"frozen_at"`
}

type agentJSON struct {
	ID        string     `json:"id"`
	AccountID string     `json:"account_id"`
	Name      string     `json:"name"`
	Status    string     `json:"status"`
	RevokedAt *time.Time `json:"revoked_at"`
}

type credentialJSON struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	RevokedAt  *time.Time `json:"revoked_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

func decode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(body, &v), string(body))
	return v
}

// myAccount is the one account the person behind cookie belongs to.
func (f *apiFixture) myAccount(cookie string) accountJSON {
	f.t.Helper()
	rec := f.serve(request{method: http.MethodGet, path: "/v1/accounts", cookie: cookie})
	require.Equal(f.t, http.StatusOK, rec.Code, rec.Body.String())
	got := decode[struct {
		Accounts []accountJSON `json:"accounts"`
	}](f.t, rec.Body.Bytes())
	require.Len(f.t, got.Accounts, 1)
	return got.Accounts[0]
}

// stepUp enters the next authenticator code. The step the last code used is
// spent, so this one is for 30 seconds on, which the skew accepts.
func (f *apiFixture) stepUp(cookie, secret string, at time.Time) {
	f.t.Helper()
	rec := f.onConsoleAs(cookie, http.MethodPost, "/v1/mfa/step-up", codeJSON("code", totpCode(f.t, secret, at)))
	require.Equal(f.t, http.StatusOK, rec.Code, rec.Body.String())
}

// asAgent calls a probe route on the agent surface with key, reporting the
// status, so a test can tell whether the key authenticates.
func (f *apiFixture) asAgent(key string) int {
	return f.do(http.MethodGet, "/v1/probe/agent", "", credentials{agentKey: key}).Code
}

func (f *apiFixture) agentProbe() {
	f.api.surfaces.agent.GET("/probe/agent", func(c *gin.Context) { c.Status(http.StatusNoContent) })
}

// TestAgentRoutes_ConnectToSpendAndBack is the whole life of an agent over
// HTTP, from the account the owner signed up with.
func TestAgentRoutes_ConnectToSpendAndBack(t *testing.T) {
	f := newAPIFixture(t)
	f.agentProbe()
	cookie, secret, _ := f.enrolled("owner@example.com")

	acct := f.myAccount(cookie)
	assert.Equal(t, "owner", acct.Role)
	assert.Nil(t, acct.FrozenAt)

	rec := f.onConsoleAs(cookie, http.MethodPost, "/v1/accounts/"+acct.ID+"/agents", `{"name":"invoice-runner"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	agent := decode[agentJSON](t, rec.Body.Bytes())
	assert.Equal(t, acct.ID, agent.AccountID)
	assert.Equal(t, "invoice-runner", agent.Name)
	assert.Equal(t, "active", agent.Status)

	// Issuing a key is behind a step-up; confirming enrolment did not count.
	keys := "/v1/agents/" + agent.ID + "/credentials"
	rec = f.onConsoleAs(cookie, http.MethodPost, keys, `{"name":"prod-worker-1"}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), account.ReasonStepUpRequired)

	now := time.Now()
	f.stepUp(cookie, secret, now.Add(30*time.Second))
	rec = f.onConsoleAs(cookie, http.MethodPost, keys, `{"name":"prod-worker-1"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"), "the key must not sit in a cache")
	issued := decode[struct {
		Key        string         `json:"key"`
		Credential credentialJSON `json:"credential"`
	}](t, rec.Body.Bytes())
	assert.Contains(t, issued.Key, issued.Credential.Prefix)
	assert.Equal(t, http.StatusNoContent, f.asAgent(issued.Key), "the key authenticates the agent")

	rec = f.serve(request{method: http.MethodGet, path: keys, cookie: cookie})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	listed := decode[struct {
		Credentials []credentialJSON `json:"credentials"`
	}](t, rec.Body.Bytes())
	require.Len(t, listed.Credentials, 1)
	assert.Equal(t, issued.Credential.Prefix, listed.Credentials[0].Prefix)
	assert.NotNil(t, listed.Credentials[0].LastUsedAt, "the console can see the key is in use")
	assert.NotContains(t, rec.Body.String(), issued.Key, "listing never shows the key again")

	rec = f.onConsoleAs(cookie, http.MethodPut, "/v1/agents/"+agent.ID+"/mandate", rulesJSON)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// A second key, so rotation can retire the first without the agent going
	// down, and then the agent itself.
	rec = f.onConsoleAs(cookie, http.MethodPost, keys, `{"name":"prod-worker-2"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	second := decode[struct {
		Key string `json:"key"`
	}](t, rec.Body.Bytes())

	rec = f.onConsoleAs(cookie, http.MethodPost, keys+"/"+issued.Credential.ID+"/revoke", "")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusUnauthorized, f.asAgent(issued.Key))
	assert.Equal(t, http.StatusNoContent, f.asAgent(second.Key))

	rec = f.onConsoleAs(cookie, http.MethodPost, "/v1/agents/"+agent.ID+"/revoke", "")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusUnauthorized, f.asAgent(second.Key), "revoking the agent ends every key")

	rec = f.serve(request{method: http.MethodGet, path: "/v1/accounts/" + acct.ID + "/agents", cookie: cookie})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	agents := decode[struct {
		Agents []agentJSON `json:"agents"`
	}](t, rec.Body.Bytes())
	require.Len(t, agents.Agents, 1)
	assert.Equal(t, "revoked", agents.Agents[0].Status)
	assert.NotNil(t, agents.Agents[0].RevokedAt)
}

// TestFreezeRoutes_FreezeIsInstantAndUnfreezeNeedsAStepUp: freezing is what
// somebody does in a hurry, and undoing it must take more than a session.
func TestFreezeRoutes_FreezeIsInstantAndUnfreezeNeedsAStepUp(t *testing.T) {
	f := newAPIFixture(t)
	cookie, secret, _ := f.enrolled("owner@example.com")
	acct := f.myAccount(cookie)

	rec := f.onConsoleAs(cookie, http.MethodPost, "/v1/accounts/"+acct.ID+"/freeze", "")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.NotNil(t, f.myAccount(cookie).FrozenAt)

	rec = f.onConsoleAs(cookie, http.MethodPost, "/v1/accounts/"+acct.ID+"/unfreeze", "")
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), account.ReasonStepUpRequired)
	assert.NotNil(t, f.myAccount(cookie).FrozenAt)

	f.stepUp(cookie, secret, time.Now().Add(30*time.Second))
	rec = f.onConsoleAs(cookie, http.MethodPost, "/v1/accounts/"+acct.ID+"/unfreeze", "")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Nil(t, f.myAccount(cookie).FrozenAt)
}

// TestAgentRoutes_Refusals: the answers for somebody else's account or agent,
// for a role that changes nothing, and for identifiers that name nothing.
func TestAgentRoutes_Refusals(t *testing.T) {
	f := newAPIFixture(t)
	admin := f.session("admin@example.com", "admin", true)
	viewer := f.session("viewer@example.com", "viewer", true)
	stranger := func() string {
		other := &apiFixture{t: t, db: f.db, api: f.api}
		other.seedAgent()
		return other.session("stranger@example.com", "owner", true)
	}()
	ours := "/v1/accounts/" + string(f.accountID)
	agent := "/v1/agents/" + string(f.agentID)
	unknown := "00000000-0000-0000-0000-000000000000"

	cases := map[string]struct {
		method, path, body, session string
		want                        int
	}{
		"stranger lists our agents":      {http.MethodGet, ours + "/agents", "", stranger, http.StatusForbidden},
		"stranger connects to us":        {http.MethodPost, ours + "/agents", `{"name":"rogue"}`, stranger, http.StatusForbidden},
		"stranger freezes us":            {http.MethodPost, ours + "/freeze", "", stranger, http.StatusForbidden},
		"stranger issues a key":          {http.MethodPost, agent + "/credentials", `{"name":"rogue"}`, stranger, http.StatusNotFound},
		"stranger lists keys":            {http.MethodGet, agent + "/credentials", "", stranger, http.StatusNotFound},
		"stranger revokes our agent":     {http.MethodPost, agent + "/revoke", "", stranger, http.StatusNotFound},
		"viewer connects":                {http.MethodPost, ours + "/agents", `{"name":"rogue"}`, viewer, http.StatusForbidden},
		"viewer issues a key":            {http.MethodPost, agent + "/credentials", `{"name":"rogue"}`, viewer, http.StatusForbidden},
		"viewer freezes":                 {http.MethodPost, ours + "/freeze", "", viewer, http.StatusForbidden},
		"account that is not a uuid":     {http.MethodGet, "/v1/accounts/acme/agents", "", admin, http.StatusForbidden},
		"unknown account":                {http.MethodGet, "/v1/accounts/" + unknown + "/agents", "", admin, http.StatusForbidden},
		"agent that is not a uuid":       {http.MethodPost, "/v1/agents/buyer/revoke", "", admin, http.StatusNotFound},
		"unknown agent":                  {http.MethodPost, "/v1/agents/" + unknown + "/revoke", "", admin, http.StatusNotFound},
		"credential that is not a uuid":  {http.MethodPost, agent + "/credentials/prod/revoke", "", admin, http.StatusNotFound},
		"unknown credential":             {http.MethodPost, agent + "/credentials/" + unknown + "/revoke", "", admin, http.StatusNotFound},
		"agent with no name":             {http.MethodPost, ours + "/agents", `{"name":"  "}`, admin, http.StatusBadRequest},
		"key with no name":               {http.MethodPost, agent + "/credentials", `{}`, admin, http.StatusBadRequest},
		"connect that is not json":       {http.MethodPost, ours + "/agents", `{`, admin, http.StatusBadRequest},
		"agent name past the length cap": {http.MethodPost, ours + "/agents", `{"name":"` + longName + `"}`, admin, http.StatusBadRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := f.do(tc.method, tc.path, tc.body, credentials{sessionToken: tc.session, consoleHeader: true})
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
		})
	}

	// Nothing above changed anything.
	var agents, keys int
	var frozen bool
	require.NoError(t, f.db.QueryRow(t.Context(), `
		SELECT (SELECT count(*) FROM agent WHERE account_id = $1),
		       (SELECT count(*) FROM agent_credential c JOIN agent a ON a.id = c.agent_id
		         WHERE a.account_id = $1 AND c.revoked_at IS NULL),
		       (SELECT frozen_at IS NOT NULL FROM account WHERE id = $1)`,
		string(f.accountID)).Scan(&agents, &keys, &frozen))
	assert.Equal(t, 1, agents)
	assert.Equal(t, 1, keys)
	assert.False(t, frozen)
	assert.Equal(t, http.StatusNoContent, func() int { f.agentProbe(); return f.asAgent(f.agentKey) }())
}

var longName = strings.Repeat("a", account.MaxAgentNameLength+1)
