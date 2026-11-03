//go:build integration

package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/crypto"
	"github.com/CleargateFinance/cleargate-core/internal/modules/account"
)

// These tests drive Task 2.4's second factor through the console surface: a
// person who signed up over HTTP enrols, steps up and then approves a mandate,
// which is the whole path to an agent being allowed to spend.

// totpCode is what an authenticator app shows at at.
func totpCode(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := hotp.GenerateCodeCustom(secret, uint64(crypto.TOTPStep(at)), hotp.ValidateOpts{
		Digits: crypto.TOTPDigits, Algorithm: otp.AlgorithmSHA1,
	})
	require.NoError(t, err)
	return code
}

// onConsoleAs sends a console request that changes something.
func (f *apiFixture) onConsoleAs(cookie, method, path, body string) *httptest.ResponseRecorder {
	return f.serve(request{method: method, path: path, body: body, cookie: cookie, consoleHeader: true})
}

func codeJSON(field, code string) string {
	b, _ := json.Marshal(map[string]string{field: code})
	return string(b)
}

// enrolled signs up, logs in and enrols TOTP over HTTP, returning the cookie,
// the shared secret and the recovery codes.
func (f *apiFixture) enrolled(email string) (string, string, []string) {
	f.t.Helper()
	f.signUp(email)
	cookie := f.logIn(email, password).Value

	rec := f.onConsoleAs(cookie, http.MethodPost, "/v1/mfa/totp", "")
	require.Equal(f.t, http.StatusOK, rec.Code, rec.Body.String())
	var begun struct {
		Secret string `json:"secret"`
		URI    string `json:"uri"`
	}
	require.NoError(f.t, json.Unmarshal(rec.Body.Bytes(), &begun))

	rec = f.onConsoleAs(cookie, http.MethodPost, "/v1/mfa/totp/confirm", codeJSON("code", totpCode(f.t, begun.Secret, time.Now())))
	require.Equal(f.t, http.StatusOK, rec.Code, rec.Body.String())
	var confirmed struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	require.NoError(f.t, json.Unmarshal(rec.Body.Bytes(), &confirmed))
	return cookie, begun.Secret, confirmed.RecoveryCodes
}

// ownAgent connects an agent to the account email's sign-up created.
func (f *apiFixture) ownAgent(email string) string {
	f.t.Helper()
	var agentID string
	require.NoError(f.t, f.db.QueryRow(context.Background(), `
		INSERT INTO agent (account_id, name)
		SELECT m.account_id, 'buyer' FROM account_membership m JOIN app_user u ON u.id = m.user_id
		WHERE u.email = $1 AND m.role = 'owner'
		RETURNING id`, email).Scan(&agentID))
	return agentID
}

func mfaEnrolled(t *testing.T, f *apiFixture, cookie string) bool {
	t.Helper()
	rec := f.serve(request{method: http.MethodGet, path: "/v1/mfa", cookie: cookie})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got struct {
		TOTP bool `json:"totp"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	return got.TOTP
}

// TestMFARoutes_SignUpToApprovedMandate is the path a new customer takes
// before any agent may spend: sign up, enrol, step up, approve.
func TestMFARoutes_SignUpToApprovedMandate(t *testing.T) {
	f := newAPIFixture(t)
	f.signUp("owner@example.com")
	cookie := f.logIn("owner@example.com", password).Value
	mandate := "/v1/agents/" + f.ownAgent("owner@example.com") + "/mandate"

	assert.False(t, mfaEnrolled(t, f, cookie))
	rec := f.onConsoleAs(cookie, http.MethodPut, mandate, rulesJSON)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), account.ReasonMFARequired, "the console is told to start enrolment")

	rec = f.onConsoleAs(cookie, http.MethodPost, "/v1/mfa/totp", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"), "the secret must not sit in a cache")
	var begun struct {
		Secret string `json:"secret"`
		URI    string `json:"uri"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &begun))
	uri, err := url.Parse(begun.URI)
	require.NoError(t, err)
	assert.Equal(t, "otpauth", uri.Scheme)
	assert.Contains(t, uri.Path, "owner@example.com", "the app lists the code under the person's address")
	assert.False(t, mfaEnrolled(t, f, cookie), "an unconfirmed factor counts for nothing")

	now := time.Now()
	rec = f.onConsoleAs(cookie, http.MethodPost, "/v1/mfa/totp/confirm", codeJSON("code", totpCode(t, begun.Secret, now)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	var confirmed struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &confirmed))
	assert.Len(t, confirmed.RecoveryCodes, account.RecoveryCodeCount)
	assert.True(t, mfaEnrolled(t, f, cookie))

	rec = f.onConsoleAs(cookie, http.MethodPut, mandate, rulesJSON)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), account.ReasonStepUpRequired, "the console is told to ask for a code")

	// The confirming code's step is spent, so the step-up uses the next one,
	// which the ±1 step skew accepts.
	rec = f.onConsoleAs(cookie, http.MethodPost, "/v1/mfa/step-up", codeJSON("code", totpCode(t, begun.Secret, now.Add(30*time.Second))))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"mfa_satisfied_at"`)

	rec = f.onConsoleAs(cookie, http.MethodPut, mandate, rulesJSON)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestMFARoutes_ACodeWorksOnce(t *testing.T) {
	f := newAPIFixture(t)
	cookie, secret, _ := f.enrolled("owner@example.com")
	code := codeJSON("code", totpCode(t, secret, time.Now().Add(30*time.Second)))

	require.Equal(t, http.StatusOK, f.onConsoleAs(cookie, http.MethodPost, "/v1/mfa/step-up", code).Code)
	assert.Equal(t, http.StatusUnauthorized, f.onConsoleAs(cookie, http.MethodPost, "/v1/mfa/step-up", code).Code,
		"a replayed code is refused like a wrong one")
	assert.Equal(t, http.StatusUnauthorized,
		f.onConsoleAs(cookie, http.MethodPost, "/v1/mfa/step-up", codeJSON("code", "000000")).Code)
}

func TestMFARoutes_ARecoveryCodeStepsUpOnce(t *testing.T) {
	f := newAPIFixture(t)
	cookie, _, codes := f.enrolled("owner@example.com")
	mandate := "/v1/agents/" + f.ownAgent("owner@example.com") + "/mandate"
	body := codeJSON("recovery_code", codes[0])

	rec := f.onConsoleAs(cookie, http.MethodPost, "/v1/mfa/step-up", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusOK, f.onConsoleAs(cookie, http.MethodPut, mandate, rulesJSON).Code,
		"a lost phone does not lock the owner out")
	assert.Equal(t, http.StatusUnauthorized, f.onConsoleAs(cookie, http.MethodPost, "/v1/mfa/step-up", body).Code)
}

func TestMFARoutes_Refusals(t *testing.T) {
	f := newAPIFixture(t)
	cookie, secret, _ := f.enrolled("owner@example.com")
	code := totpCode(t, secret, time.Now().Add(30*time.Second))

	cases := map[string]struct {
		path, body string
		want       int
	}{
		"enrolling over a confirmed factor": {"/v1/mfa/totp", "", http.StatusConflict},
		"confirming with nothing pending":   {"/v1/mfa/totp/confirm", codeJSON("code", code), http.StatusConflict},
		"step-up with no code":              {"/v1/mfa/step-up", `{}`, http.StatusBadRequest},
		"step-up with both kinds of code":   {"/v1/mfa/step-up", `{"code":"123456","recovery_code":"ABCDE-12345"}`, http.StatusBadRequest},
		"step-up that is not json":          {"/v1/mfa/step-up", `{`, http.StatusBadRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := f.onConsoleAs(cookie, http.MethodPost, tc.path, tc.body)
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
		})
	}
}

func TestMFARoutes_StepUpWithoutAFactor(t *testing.T) {
	f := newAPIFixture(t)
	f.signUp("owner@example.com")
	cookie := f.logIn("owner@example.com", password).Value

	rec := f.onConsoleAs(cookie, http.MethodPost, "/v1/mfa/step-up", codeJSON("code", "123456"))

	assert.Equal(t, http.StatusForbidden, rec.Code)
}
