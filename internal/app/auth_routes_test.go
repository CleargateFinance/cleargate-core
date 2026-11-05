//go:build integration

package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/config"
	"github.com/CleargateFinance/cleargate-core/internal/modules/account"
)

// These tests drive Task 2.3's sign-up, login and reset flows through the
// public surface, as the console will, and the rate limits of Task 6.1.

const password = "a perfectly reasonable passphrase"

// request is one HTTP call with full control over the parts the credentials
// helper does not cover: the peer address and any extra header.
type request struct {
	method, path, body string
	cookie             string
	consoleHeader      bool
	agentKey           string
	remoteAddr         string
	header             map[string]string
}

func (f *apiFixture) serve(r request) *httptest.ResponseRecorder {
	req := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
	req.Header.Set("Content-Type", "application/json")
	if r.cookie != "" {
		req.AddCookie(&http.Cookie{Name: account.SessionCookie, Value: r.cookie})
	}
	if r.consoleHeader {
		req.Header.Set(account.ConsoleHeader, "1")
	}
	if r.agentKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.agentKey)
	}
	if r.remoteAddr != "" {
		req.RemoteAddr = r.remoteAddr
	}
	for k, v := range r.header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.api.Engine.ServeHTTP(rec, req)
	f.conform(req, rec)
	return rec
}

// console posts to an auth route the way the console does.
func (f *apiFixture) console(path, body string) *httptest.ResponseRecorder {
	return f.serve(request{method: http.MethodPost, path: path, body: body, consoleHeader: true})
}

func credentialsJSON(email, pw string) string {
	b, _ := json.Marshal(map[string]string{"email": email, "password": pw})
	return string(b)
}

func tokenJSON(token string) string {
	b, _ := json.Marshal(map[string]string{"token": token})
	return string(b)
}

// signUp registers email and follows the verification link.
func (f *apiFixture) signUp(email string) {
	f.t.Helper()
	require.Equal(f.t, http.StatusAccepted, f.console("/v1/auth/signup", credentialsJSON(email, password)).Code)
	token := f.mail.token(email, "/verify-email")
	require.NotEmpty(f.t, token, "sign-up mails a verification link")
	rec := f.console("/v1/auth/verify-email", tokenJSON(token))
	require.Equal(f.t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// logIn signs in and returns the session cookie.
func (f *apiFixture) logIn(email, pw string) *http.Cookie {
	f.t.Helper()
	rec := f.console("/v1/auth/login", credentialsJSON(email, pw))
	require.Equal(f.t, http.StatusOK, rec.Code, rec.Body.String())
	for _, c := range rec.Result().Cookies() {
		if c.Name == account.SessionCookie {
			return c
		}
	}
	f.t.Fatal("login set no session cookie")
	return nil
}

// probeConsole adds a console route that answers 204 to a signed-in person.
func (f *apiFixture) probeConsole() {
	f.api.surfaces.console.GET("/probe/console", func(c *gin.Context) { c.Status(http.StatusNoContent) })
}

func (f *apiFixture) onConsole(cookie string) int {
	return f.serve(request{method: http.MethodGet, path: "/v1/probe/console", cookie: cookie}).Code
}

// withoutRequestID is a response body minus its request id, the one field
// that differs between any two responses.
func withoutRequestID(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Body.Len() == 0 {
		return nil
	}
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	delete(body, "request_id")
	return body
}

func TestAuthRoutes_SignUpVerifyLogInAndOut(t *testing.T) {
	f := newAPIFixture(t)
	f.probeConsole()
	f.signUp("anna@example.com")

	rec := f.console("/v1/auth/login", credentialsJSON("anna@example.com", password))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == account.SessionCookie {
			cookie = c
		}
	}
	require.NotNil(t, cookie)
	assert.True(t, cookie.HttpOnly, "no script on the console reads the session")
	assert.True(t, cookie.Secure, "never sent over plain HTTP")
	assert.Equal(t, http.SameSiteStrictMode, cookie.SameSite, "never sent on a request another site starts")
	assert.Empty(t, cookie.Domain, "host-only, so no sibling subdomain receives it")
	assert.Equal(t, "/", cookie.Path)
	assert.Equal(t, int(account.DefaultSessionLifetime.Seconds()), cookie.MaxAge)
	assert.NotContains(t, rec.Body.String(), cookie.Value, "the token travels only in the cookie")
	assert.Contains(t, rec.Body.String(), `"expires_at"`)

	assert.Equal(t, http.StatusNoContent, f.onConsole(cookie.Value), "the session opens the console")

	rec = f.serve(request{method: http.MethodPost, path: "/v1/auth/logout", cookie: cookie.Value, consoleHeader: true})
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	cleared := rec.Result().Cookies()
	require.Len(t, cleared, 1)
	assert.Equal(t, account.SessionCookie, cleared[0].Name)
	assert.Negative(t, cleared[0].MaxAge, "logout deletes the cookie")

	assert.Equal(t, http.StatusUnauthorized, f.onConsole(cookie.Value), "and ends the session itself")
}

func TestAuthRoutes_LoginRefusesAnUnverifiedAddress(t *testing.T) {
	f := newAPIFixture(t)
	require.Equal(t, http.StatusAccepted, f.console("/v1/auth/signup", credentialsJSON("new@example.com", password)).Code)

	rec := f.console("/v1/auth/login", credentialsJSON("new@example.com", password))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Empty(t, rec.Result().Cookies())
}

// TestAuthRoutes_AnswerAlikeForKnownAndUnknownAddresses is Task 2.3's
// enumeration defence seen from outside: status and body are the same.
func TestAuthRoutes_AnswerAlikeForKnownAndUnknownAddresses(t *testing.T) {
	f := newAPIFixture(t)
	f.signUp("known@example.com")

	pairs := map[string][2]*httptest.ResponseRecorder{
		"sign-up": {
			f.console("/v1/auth/signup", credentialsJSON("known@example.com", password)),
			f.console("/v1/auth/signup", credentialsJSON("unknown@example.com", password)),
		},
		"login": {
			f.console("/v1/auth/login", credentialsJSON("known@example.com", "not the passphrase")),
			f.console("/v1/auth/login", credentialsJSON("unknown@example.com", "not the passphrase")),
		},
		"reset request": {
			f.console("/v1/auth/password/reset-request", `{"email":"known@example.com"}`),
			f.console("/v1/auth/password/reset-request", `{"email":"unknown@example.com"}`),
		},
	}
	for name, p := range pairs {
		t.Run(name, func(t *testing.T) {
			known, unknown := p[0], p[1]
			assert.Equal(t, unknown.Code, known.Code)
			assert.Equal(t, withoutRequestID(t, unknown), withoutRequestID(t, known))
		})
	}
}

func TestAuthRoutes_PasswordResetEndsEverySession(t *testing.T) {
	f := newAPIFixture(t)
	f.probeConsole()
	f.signUp("anna@example.com")
	old := f.logIn("anna@example.com", password)

	require.Equal(t, http.StatusAccepted,
		f.console("/v1/auth/password/reset-request", `{"email":"anna@example.com"}`).Code)
	token := f.mail.token("anna@example.com", "/reset-password")
	require.NotEmpty(t, token)

	body, _ := json.Marshal(map[string]string{"token": token, "password": "an entirely new passphrase"})
	rec := f.console("/v1/auth/password/reset", string(body))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	assert.Equal(t, http.StatusUnauthorized, f.onConsole(old.Value), "a reset signs out every device")
	assert.Equal(t, http.StatusUnauthorized, f.console("/v1/auth/password/reset", string(body)).Code, "the link works once")
	f.logIn("anna@example.com", "an entirely new passphrase")
}

// TestAuthRoutes_NeedTheConsoleHeader: without it, another site could post a
// form that signs the visitor into the attacker's account.
func TestAuthRoutes_NeedTheConsoleHeader(t *testing.T) {
	f := newAPIFixture(t)
	f.signUp("anna@example.com")

	for _, path := range []string{
		"/v1/auth/signup", "/v1/auth/verify-email", "/v1/auth/login", "/v1/auth/logout",
		"/v1/auth/password/reset-request", "/v1/auth/password/reset",
	} {
		t.Run(path, func(t *testing.T) {
			rec := f.serve(request{method: http.MethodPost, path: path, body: credentialsJSON("anna@example.com", password)})
			assert.Equal(t, http.StatusForbidden, rec.Code)
			assert.Empty(t, rec.Result().Cookies())
		})
	}
}

func TestAuthRoutes_RefuseAnOversizedBody(t *testing.T) {
	f := newAPIFixture(t)

	// A sign-up that is valid apart from its size, so only the bound refuses it.
	rec := f.console("/v1/auth/signup",
		`{"email":"a@example.com","password":"`+password+`","padding":"`+strings.Repeat("p", 32<<10)+`"}`)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, f.mail.token("a@example.com", "/verify-email"))
}

func TestAuthRoutes_LoginSurvivesAUserAgentPostgresCannotStore(t *testing.T) {
	f := newAPIFixture(t)
	f.signUp("anna@example.com")

	rec := f.serve(request{
		method: http.MethodPost, path: "/v1/auth/login", consoleHeader: true,
		body:   credentialsJSON("anna@example.com", password),
		header: map[string]string{"User-Agent": "agent\xff/" + strings.Repeat("x", 4096)},
	})

	require.Equal(t, http.StatusOK, rec.Code, "a header Postgres cannot store must not stop a sign-in")
	var stored string
	require.NoError(t, f.db.QueryRow(t.Context(), `SELECT user_agent FROM user_session`).Scan(&stored))
	assert.Len(t, stored, 512)
	assert.True(t, strings.HasPrefix(stored, "agent/x"))
}

// TestRateLimit_PublicIsPerClientAddress: a forwarded header from a peer that
// is not a trusted proxy does not buy a fresh allowance.
func TestRateLimit_PublicIsPerClientAddress(t *testing.T) {
	f := newAPIFixture(t, func(c *config.Config) { c.RateLimit.Public = 3 })
	from := func(addr, forwarded string) int {
		r := request{method: http.MethodPost, path: "/v1/auth/login", body: "{", consoleHeader: true, remoteAddr: addr + ":40000"}
		if forwarded != "" {
			r.header = map[string]string{"X-Forwarded-For": forwarded}
		}
		return f.serve(r).Code
	}

	for range 3 {
		assert.Equal(t, http.StatusBadRequest, from("203.0.113.1", ""))
	}
	rec := f.serve(request{method: http.MethodPost, path: "/v1/auth/login", body: "{", consoleHeader: true, remoteAddr: "203.0.113.1:40000"})
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
	assert.Equal(t, http.StatusTooManyRequests, from("203.0.113.1", "198.51.100.7"), "a client cannot name its own address")
	assert.Equal(t, http.StatusBadRequest, from("203.0.113.2", ""), "another address has its own allowance")

	for range 5 {
		assert.Equal(t, http.StatusOK, f.serve(request{method: http.MethodGet, path: "/v1/health", remoteAddr: "203.0.113.1:40000"}).Code,
			"health is outside the limits, or a load balancer would drain healthy instances")
	}
}

func TestRateLimit_PublicBelievesATrustedProxy(t *testing.T) {
	f := newAPIFixture(t, func(c *config.Config) {
		c.RateLimit.Public = 1
		c.Server.TrustedProxies = []string{"10.0.0.0/8"}
	})
	via := func(client string) int {
		return f.serve(request{
			method: http.MethodPost, path: "/v1/auth/login", body: "{", consoleHeader: true,
			remoteAddr: "10.0.0.5:40000", header: map[string]string{"X-Forwarded-For": client},
		}).Code
	}

	assert.Equal(t, http.StatusBadRequest, via("198.51.100.1"))
	assert.Equal(t, http.StatusBadRequest, via("198.51.100.2"), "clients behind one load balancer are counted apart")
	assert.Equal(t, http.StatusTooManyRequests, via("198.51.100.1"))
}

func TestRateLimit_AgentIsPerAgent(t *testing.T) {
	f := newAPIFixture(t, func(c *config.Config) { c.RateLimit.Agent = 2 })
	f.api.surfaces.agent.GET("/probe/agent", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	other := &apiFixture{t: t, db: f.db, api: f.api}
	other.seedAgent()
	call := func(key string) int {
		return f.serve(request{method: http.MethodGet, path: "/v1/probe/agent", agentKey: key}).Code
	}

	assert.Equal(t, http.StatusNoContent, call(f.agentKey))
	assert.Equal(t, http.StatusNoContent, call(f.agentKey))
	assert.Equal(t, http.StatusTooManyRequests, call(f.agentKey))
	assert.Equal(t, http.StatusNoContent, call(other.agentKey), "another agent has its own allowance")
}

func TestRateLimit_ConsoleIsPerPerson(t *testing.T) {
	f := newAPIFixture(t, func(c *config.Config) { c.RateLimit.Console = 2 })
	f.probeConsole()
	anna := f.session("anna@example.com", "admin", false)
	ben := f.session("ben@example.com", "viewer", false)

	assert.Equal(t, http.StatusNoContent, f.onConsole(anna))
	assert.Equal(t, http.StatusNoContent, f.onConsole(anna))
	assert.Equal(t, http.StatusTooManyRequests, f.onConsole(anna))
	assert.Equal(t, http.StatusNoContent, f.onConsole(ben), "another person has their own allowance")
}
