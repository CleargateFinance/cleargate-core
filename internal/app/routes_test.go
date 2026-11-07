//go:build integration

package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/cdp/cdptest"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/config"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/crypto"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/solana/solanatest"
	"github.com/CleargateFinance/cleargate-core/internal/modules/account"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

// These tests build the API through BuildAPI, as cmd/api does, and check the
// surfaces of Task 6.1: each authenticated surface accepts only its own kind
// of credential, and nothing that changes a mandate is reachable with an
// agent's key.

type apiFixture struct {
	t    *testing.T
	db   *fixtures.TestDB
	api  *API
	mail *mailLog
	cdp  *cdptest.Fake
	cfg  *config.Config

	accountID id.AccountID
	agentID   id.AgentID
	agentKey  string
}

// newAPIFixture builds the API with limits high enough that no test meets
// them by accident. configure may change the config before the build.
func newAPIFixture(t *testing.T, configure ...func(*config.Config)) *apiFixture {
	t.Helper()
	db := fixtures.NewTestDB(t)
	redis := fixtures.NewTestCache(t)

	key := make([]byte, crypto.MasterKeySize)
	_, err := rand.Read(key)
	require.NoError(t, err)

	custodian := cdptest.New(t)
	cfg := &config.Config{
		CDP:       custodian.Config(),
		Solana:    config.Solana{Network: devnetRail.Network, USDCMint: solanatest.DevnetUSDC, USDCDecimals: 6},
		Server:    config.Server{Mode: "debug"},
		RateLimit: config.RateLimit{Public: 1000, Agent: 1000, Console: 1000},
		Database:  config.Database{DSN: db.DSN, MaxConns: 4, MinConns: 1, ConnectTimeout: 10 * time.Second},
		Cache:     config.Cache{Addr: redis.Addr},
		Crypto:    config.Crypto{MasterKeys: map[byte][]byte{1: key}, ActiveKeyID: 1},
		Mail:      config.Mail{Transport: "log", ConsoleURL: "http://localhost:3000"},
		Log:       config.Log{Level: "error", Format: "text"},
	}
	for _, c := range configure {
		c(cfg)
	}

	mail := &mailLog{tokens: map[string]string{}}
	api, cleanup, err := BuildAPI(context.Background(), cfg, slog.New(mail))
	require.NoError(t, err)
	t.Cleanup(cleanup)

	f := &apiFixture{t: t, db: db, api: api, mail: mail, cdp: custodian, cfg: cfg}
	f.seedAgent()
	return f
}

// mailLog is the API's logger. The logging mailer writes each link it would
// have sent as a "link" attribute, so a test can follow it as the recipient
// would. Everything else logged is dropped.
type mailLog struct {
	mu     sync.Mutex
	tokens map[string]string // "<to> <path>" -> token
}

func (m *mailLog) Enabled(context.Context, slog.Level) bool { return true }
func (m *mailLog) WithAttrs([]slog.Attr) slog.Handler       { return m }
func (m *mailLog) WithGroup(string) slog.Handler            { return m }

func (m *mailLog) Handle(_ context.Context, r slog.Record) error {
	var to, link string
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "to":
			to = a.Value.String()
		case "link":
			link = a.Value.String()
		}
		return true
	})
	if link == "" {
		return nil
	}
	u, err := url.Parse(link)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[to+" "+u.Path] = u.Query().Get("token")
	return nil
}

// token is the token in the last link of kind path ("/verify-email",
// "/reset-password") mailed to to, or "" if none was.
func (m *mailLog) token(to, path string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokens[to+" "+path]
}

// seedAgent creates an account with one agent holding one live API key.
func (f *apiFixture) seedAgent() {
	ctx := context.Background()
	var accountID, agentID string
	require.NoError(f.t, f.db.QueryRow(ctx, `
		WITH p AS (INSERT INTO principal (kind) VALUES ('person') RETURNING id),
		     a AS (INSERT INTO account (principal_id, display_name) SELECT id, 'acme' FROM p RETURNING id)
		INSERT INTO agent (account_id, name) SELECT id, 'buyer' FROM a
		RETURNING account_id, id`).Scan(&accountID, &agentID))

	key, err := crypto.NewAPIKey()
	require.NoError(f.t, err)
	_, err = f.db.Exec(ctx,
		`INSERT INTO agent_credential (agent_id, name, prefix, secret_hash) VALUES ($1, 'prod', $2, $3)`,
		agentID, key.Prefix(), key.Digest())
	require.NoError(f.t, err)

	f.accountID, f.agentID, f.agentKey = id.AccountID(accountID), id.AgentID(agentID), key.Plaintext()
}

// session signs in a new person holding role in the fixture's account, with a
// confirmed second factor, and returns their session token. stepUp says
// whether the session entered that factor at login.
func (f *apiFixture) session(email, role string, stepUp bool) string {
	ctx := context.Background()
	var userID string
	require.NoError(f.t, f.db.QueryRow(ctx, `
		WITH p AS (INSERT INTO principal (kind) VALUES ('person') RETURNING id)
		INSERT INTO app_user (principal_id, email, password_hash) SELECT id, $1, 'x' FROM p
		RETURNING id`, email).Scan(&userID))
	_, err := f.db.Exec(ctx,
		`INSERT INTO account_membership (account_id, user_id, role) VALUES ($1, $2, $3)`,
		string(f.accountID), userID, role)
	require.NoError(f.t, err)
	_, err = f.db.Exec(ctx,
		`INSERT INTO user_mfa_factor (user_id, type, secret_enc, confirmed_at) VALUES ($1, 'totp', 'sealed', now())`,
		userID)
	require.NoError(f.t, err)

	token, _, err := f.api.accounts.Service().IssueSession(ctx, account.NewSessionParams{
		UserID: id.UserID(userID), MFASatisfied: stepUp,
	})
	require.NoError(f.t, err)
	return token.Plaintext()
}

type credentials struct {
	agentKey      string
	sessionToken  string
	consoleHeader bool
}

func (f *apiFixture) do(method, path, body string, creds credentials) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if creds.agentKey != "" {
		req.Header.Set("Authorization", "Bearer "+creds.agentKey)
	}
	if creds.sessionToken != "" {
		req.AddCookie(&http.Cookie{Name: account.SessionCookie, Value: creds.sessionToken})
	}
	if creds.consoleHeader {
		req.Header.Set(account.ConsoleHeader, "1")
	}
	rec := httptest.NewRecorder()
	f.api.Engine.ServeHTTP(rec, req)
	f.conform(req, rec)
	return rec
}

const rulesJSON = `{"asset":"USD","max_per_tx":"20","max_per_day":"100","max_per_month":"200",` +
	`"categories":["data_api"],"pay_to":[],"expires_at":"2030-01-01T00:00:00Z"}`

func (f *apiFixture) mandateCount() int {
	var n int
	require.NoError(f.t, f.db.QueryRow(context.Background(), `SELECT count(*) FROM mandate`).Scan(&n))
	return n
}

// TestAgentCredentialCannotReachMandateRoutes: the test Task 6.1 names. A
// valid agent key, sent every way an agent could send it, never changes a
// mandate; its own agent's included.
func TestAgentCredentialCannotReachMandateRoutes(t *testing.T) {
	f := newAPIFixture(t)
	path := "/v1/agents/" + string(f.agentID) + "/mandate"

	for name, creds := range map[string]credentials{
		"bearer key":                     {agentKey: f.agentKey},
		"bearer key with console header": {agentKey: f.agentKey, consoleHeader: true},
		"key in the session cookie":      {sessionToken: f.agentKey, consoleHeader: true},
	} {
		t.Run(name, func(t *testing.T) {
			rec := f.do(http.MethodPut, path, rulesJSON, creds)
			assert.Contains(t, []int{http.StatusUnauthorized, http.StatusNotFound}, rec.Code, rec.Body.String())
		})
	}
	assert.Zero(t, f.mandateCount())
}

// TestSurfaces_EachTakesOnlyItsOwnCredential checks every route the API has,
// including any added after this test was written: none answers without a
// credential except the public ones, and none accepts both kinds. A probe
// route on each surface keeps the check meaningful while a surface has no
// real routes yet.
func TestSurfaces_EachTakesOnlyItsOwnCredential(t *testing.T) {
	f := newAPIFixture(t)
	probe := func(c *gin.Context) { c.Status(http.StatusNoContent) }
	f.api.surfaces.agent.GET("/probe/agent", probe)
	f.api.surfaces.console.GET("/probe/console", probe)
	session := f.session("admin@example.com", "admin", true)

	// The walk sends a session to every console route, revoke and freeze
	// included, so it uses a person from another account. They authenticate,
	// which is all the walk asks, and change nothing here.
	other := &apiFixture{t: t, db: f.db, api: f.api}
	other.seedAgent()
	stranger := other.session("stranger@example.com", "owner", true)

	// Adding a route here is a decision that anyone may call it.
	public := map[string]bool{
		"GET /v1/health":                       true,
		"POST /v1/auth/signup":                 true,
		"POST /v1/auth/verify-email":           true,
		"POST /v1/auth/login":                  true,
		"POST /v1/auth/logout":                 true,
		"POST /v1/auth/password/reset-request": true,
		"POST /v1/auth/password/reset":         true,
	}
	authenticated := func(code int) bool { return code != http.StatusUnauthorized }

	routes := f.api.Engine.Routes()
	require.NotEmpty(t, routes)
	for _, r := range routes {
		name := r.Method + " " + r.Path
		path := strings.NewReplacer(":id", string(f.agentID), ":cid", "00000000-0000-0000-0000-000000000000").Replace(r.Path)
		t.Run(name, func(t *testing.T) {
			none := f.do(r.Method, path, "", credentials{})
			if public[name] {
				// Only the anonymous request: sending the session to logout
				// would end it for the routes after this one.
				assert.True(t, authenticated(none.Code), "public routes need no credential")
				return
			}

			asAgent := f.do(r.Method, path, "", credentials{agentKey: f.agentKey})
			asUser := f.do(r.Method, path, "", credentials{sessionToken: stranger, consoleHeader: true})
			assert.Equal(t, http.StatusUnauthorized, none.Code, "no credential, no answer")
			assert.False(t, authenticated(asAgent.Code) && authenticated(asUser.Code),
				"a route that accepts both an agent key and a session joins the two surfaces")
			assert.True(t, authenticated(asAgent.Code) || authenticated(asUser.Code),
				"a route that accepts neither is unreachable")
		})
	}

	assert.Equal(t, http.StatusNoContent, f.do(http.MethodGet, "/v1/probe/agent", "", credentials{agentKey: f.agentKey}).Code)
	assert.Equal(t, http.StatusNoContent, f.do(http.MethodGet, "/v1/probe/console", "", credentials{sessionToken: session}).Code)
}

// TestConsole_ChangesNeedTheConsoleHeader: the CSRF defence. A forged
// cross-site request carries the cookie but cannot add the header.
func TestConsole_ChangesNeedTheConsoleHeader(t *testing.T) {
	f := newAPIFixture(t)
	session := f.session("admin@example.com", "admin", true)

	rec := f.do(http.MethodPut, "/v1/agents/"+string(f.agentID)+"/mandate", rulesJSON, credentials{sessionToken: session})

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Zero(t, f.mandateCount())
}

func TestMandateRoute_IssuesBehindAStepUp(t *testing.T) {
	f := newAPIFixture(t)
	path := "/v1/agents/" + string(f.agentID) + "/mandate"
	admin := f.session("admin@example.com", "admin", true)

	rec := f.do(http.MethodPut, path, rulesJSON, credentials{sessionToken: admin, consoleHeader: true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var got struct {
		AgentID string `json:"agent_id"`
		Version int    `json:"version"`
		Rules   struct {
			Asset    string   `json:"asset"`
			MaxPerTx string   `json:"max_per_tx"`
			PayTo    []string `json:"pay_to"`
		} `json:"rules"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, string(f.agentID), got.AgentID)
	assert.Equal(t, 1, got.Version)
	assert.Equal(t, "USD", got.Rules.Asset)
	assert.Equal(t, "20", got.Rules.MaxPerTx)
	assert.NotNil(t, got.Rules.PayTo, "an empty allowlist is [], not null")

	rec = f.do(http.MethodPut, path, rulesJSON, credentials{sessionToken: admin, consoleHeader: true})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 2, f.mandateCount(), "a change is a new version")
}

func TestMandateRoute_Refusals(t *testing.T) {
	f := newAPIFixture(t)
	path := "/v1/agents/" + string(f.agentID) + "/mandate"
	admin := f.session("admin@example.com", "admin", true)
	noStepUp := f.session("late@example.com", "admin", false)
	viewer := f.session("viewer@example.com", "viewer", true)
	stranger := func() string {
		// A person in another account altogether.
		other := &apiFixture{t: t, db: f.db, api: f.api}
		other.seedAgent()
		return other.session("stranger@example.com", "owner", true)
	}()

	cases := map[string]struct {
		path, body, session string
		want                int
	}{
		"no recent step-up":    {path, rulesJSON, noStepUp, http.StatusForbidden},
		"viewer":               {path, rulesJSON, viewer, http.StatusForbidden},
		"another account":      {path, rulesJSON, stranger, http.StatusNotFound},
		"not a uuid":           {"/v1/agents/not-a-uuid/mandate", rulesJSON, admin, http.StatusNotFound},
		"unknown agent":        {"/v1/agents/00000000-0000-0000-0000-000000000000/mandate", rulesJSON, admin, http.StatusNotFound},
		"not json":             {path, "{", admin, http.StatusBadRequest},
		"cap as a json number": {path, strings.Replace(rulesJSON, `"20"`, `20`, 1), admin, http.StatusBadRequest},
		"not usd":              {path, strings.ReplaceAll(rulesJSON, `"USD"`, `"EUR"`), admin, http.StatusBadRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := f.do(http.MethodPut, tc.path, tc.body, credentials{sessionToken: tc.session, consoleHeader: true})
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
		})
	}
	assert.Zero(t, f.mandateCount())
}

func TestBuildAPI_RefusesTheLoggingMailerInRelease(t *testing.T) {
	_, err := newMailer(&config.Config{Server: config.Server{Mode: "release"}, Mail: config.Mail{Transport: "log"}}, nil)
	assert.ErrorIs(t, err, errNoMailTransport)

	_, err = newMailer(&config.Config{Server: config.Server{Mode: "debug"}}, nil)
	assert.ErrorIs(t, err, errNoMailTransport, "no transport is chosen by default")
}
