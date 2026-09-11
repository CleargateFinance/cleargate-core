//go:build integration

package account

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/crypto"
	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

// captureMailer records what would have been sent, so a test can follow a link
// the way a real recipient would.
type captureMailer struct {
	verifications map[string]string
	resets        map[string]string
}

func newCaptureMailer() *captureMailer {
	return &captureMailer{
		verifications: map[string]string{},
		resets:        map[string]string{},
	}
}

func (m *captureMailer) SendEmailVerification(_ context.Context, to, token string) error {
	m.verifications[to] = token
	return nil
}

func (m *captureMailer) SendPasswordReset(_ context.Context, to, token string) error {
	m.resets[to] = token
	return nil
}

// authFixture is a service wired to a throwaway Postgres and Redis, with the
// mailer captured so emailed links can be followed.
type authFixture struct {
	db     *fixtures.TestDB
	cache  *fixtures.TestCache
	svc    *Service
	repo   Repository
	mailer *captureMailer
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()

	db := fixtures.NewTestDB(t)
	redis := fixtures.NewTestCache(t)

	repo := NewPostgresRepository(db.DB)
	mailer := newCaptureMailer()

	svc := NewService(repo, cheapHasher(), redis.Client, mailer, testSealer(t),
		NewCacheThrottle(redis.Client, redis.Client, nil), nil, nil, Options{
			ConsentVersions: map[string]string{
				DocumentTermsOfService: "2026-01-01",
				DocumentPrivacyPolicy:  "2026-01-01",
			},
		})

	return &authFixture{db: db, cache: redis, svc: svc, repo: repo, mailer: mailer}
}

const goodPassword = "a perfectly reasonable passphrase"

// signupAndVerify runs the whole onboarding path so a test can start from a
// user who is able to sign in.
func (f *authFixture) signupAndVerify(t *testing.T, email string) id.UserID {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, f.svc.Signup(ctx, SignupParams{
		Email: email, Password: goodPassword, IP: "203.0.113.1",
	}))

	token, ok := f.mailer.verifications[email]
	require.True(t, ok, "sign-up must send a verification link")
	require.NoError(t, f.svc.VerifyEmail(ctx, token, "203.0.113.1", "test"))

	user, err := f.repo.UserByEmail(ctx, email)
	require.NoError(t, err)

	// Sign-up left password_changed_at at the database's now(), while any
	// session these tests open takes its issued_at from the application clock.
	// A session is valid only when issued_at >= password_changed_at, so with
	// the two instants milliseconds apart the comparison turns on however far
	// the container's clock has drifted from the host's. Backdating removes
	// the coincidence, and matches real use, where somebody reads an email
	// between setting a password and signing in.
	_, err = f.db.Exec(ctx,
		`UPDATE app_user SET password_changed_at = now() - interval '1 minute' WHERE id = $1`,
		string(user.ID))
	require.NoError(t, err)

	return user.ID
}

// authEventCount reports how many audit rows of a type exist for an address.
func (f *authFixture) authEventCount(t *testing.T, email string, eventType AuthEventType) int {
	t.Helper()

	var n int
	require.NoError(t, f.db.QueryRow(context.Background(),
		`SELECT count(*) FROM auth_event WHERE email = $1 AND type = $2`,
		email, string(eventType)).Scan(&n))
	return n
}

func TestSignup_CreatesUserAccountAndOwnerMembership(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)

	require.NoError(t, f.svc.Signup(ctx, SignupParams{
		Email: "founder@example.com", Password: goodPassword, IP: "203.0.113.1",
	}))

	user, err := f.repo.UserByEmail(ctx, "founder@example.com")
	require.NoError(t, err)
	assert.False(t, user.EmailVerified(), "a new address starts unverified")

	// The account exists and the person who created it owns it.
	var role string
	require.NoError(t, f.db.QueryRow(ctx, `
		SELECT m.role FROM account_membership m
		JOIN account a ON a.id = m.account_id
		WHERE m.user_id = $1`, string(user.ID)).Scan(&role))
	assert.Equal(t, string(RoleOwner), role)

	// The display name defaults to the local part rather than asking for one.
	var displayName string
	require.NoError(t, f.db.QueryRow(ctx, `
		SELECT a.display_name FROM account a
		JOIN account_membership m ON m.account_id = a.id
		WHERE m.user_id = $1`, string(user.ID)).Scan(&displayName))
	assert.Equal(t, "founder", displayName)

	// Consent is recorded against the versions that were on screen.
	var consents int
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT count(*) FROM user_consent WHERE user_id = $1`,
		string(user.ID)).Scan(&consents))
	assert.Equal(t, 2, consents, "terms and privacy policy are both recorded")
}

// TestSignup_DoesNotRevealExistingEmail is the property that stops this
// endpoint from confirming which addresses on a leaked list are customers.
func TestSignup_DoesNotRevealExistingEmail(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)

	first := SignupParams{Email: "taken@example.com", Password: goodPassword}
	require.NoError(t, f.svc.Signup(ctx, first))

	// A second sign-up for the same address must answer exactly as the first
	// did, with no error and nothing that distinguishes the two.
	second := SignupParams{Email: "taken@example.com", Password: "an entirely different passphrase"}
	require.NoError(t, f.svc.Signup(ctx, second),
		"a taken address must not be reported to the caller")

	// And nothing may have changed underneath: no second account, and the
	// original password still works.
	var users, accounts int
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT count(*) FROM app_user WHERE email = $1`, "taken@example.com").Scan(&users))
	require.NoError(t, f.db.QueryRow(ctx, `SELECT count(*) FROM account`).Scan(&accounts))
	assert.Equal(t, 1, users)
	assert.Equal(t, 1, accounts, "the second attempt must not have created anything")

	user, err := f.repo.UserByEmail(ctx, "taken@example.com")
	require.NoError(t, err)
	ok, err := f.svc.VerifyPassword(user.PasswordHash, goodPassword)
	require.NoError(t, err)
	assert.True(t, ok, "the second attempt must not have overwritten the password")

	// The second attempt must not have sent a link either, or the real owner
	// would get an unexplained verification email.
	assert.Len(t, f.mailer.verifications, 1)
}

func TestSignup_RejectsWeakOrMalformedInput(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)

	cases := map[string]SignupParams{
		"short password":  {Email: "a@example.com", Password: "short"},
		"empty email":     {Email: "", Password: goodPassword},
		"malformed email": {Email: "not-an-address", Password: goodPassword},
		"email with name": {Email: "Anna <anna@example.com>", Password: goodPassword},
	}

	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			err := f.svc.Signup(ctx, params)
			require.Error(t, err)
			assert.Equal(t, apperr.KindInvalid, apperr.KindOf(err),
				"malformed input is the caller's mistake and should say so")
		})
	}
}

func TestSignup_NormalisesEmailCase(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)

	require.NoError(t, f.svc.Signup(ctx, SignupParams{
		Email: "  Mixed.Case@Example.COM  ", Password: goodPassword,
	}))

	// Stored lowercase and trimmed, so the same person cannot end up with two
	// logins that differ only in case.
	user, err := f.repo.UserByEmail(ctx, "mixed.case@example.com")
	require.NoError(t, err)
	assert.Equal(t, "mixed.case@example.com", user.Email)
}

func TestVerifyEmail_IsSingleUse(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)

	require.NoError(t, f.svc.Signup(ctx, SignupParams{
		Email: "verify@example.com", Password: goodPassword,
	}))
	token := f.mailer.verifications["verify@example.com"]
	require.NotEmpty(t, token)

	require.NoError(t, f.svc.VerifyEmail(ctx, token, "", ""))

	// A forwarded link must not work a second time.
	err := f.svc.VerifyEmail(ctx, token, "", "")
	assert.ErrorIs(t, err, ErrTokenInvalid)

	// Nor must an invented one.
	err = f.svc.VerifyEmail(ctx, "not-a-real-token", "", "")
	assert.ErrorIs(t, err, ErrTokenInvalid)
}

func TestLogin_SucceedsAfterVerification(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)
	userID := f.signupAndVerify(t, "user@example.com")

	token, session, err := f.svc.Login(ctx, LoginParams{
		Email: "user@example.com", Password: goodPassword,
		IP: "203.0.113.9", UserAgent: "console/1.0",
	})
	require.NoError(t, err)
	assert.Equal(t, userID, session.UserID)

	// The returned token must be a working session immediately.
	validated, err := f.svc.ValidateSession(ctx, token.Plaintext())
	require.NoError(t, err)
	assert.Equal(t, userID, validated.UserID)

	assert.Equal(t, 1, f.authEventCount(t, "user@example.com", EventLoginSucceeded))
}

// TestLogin_RejectsUnverifiedEmail keeps a typo'd or borrowed address from
// becoming a usable login.
func TestLogin_RejectsUnverifiedEmail(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)

	require.NoError(t, f.svc.Signup(ctx, SignupParams{
		Email: "unverified@example.com", Password: goodPassword,
	}))

	_, _, err := f.svc.Login(ctx, LoginParams{
		Email: "unverified@example.com", Password: goodPassword,
	})
	assert.ErrorIs(t, err, ErrEmailNotVerified)

	// Specific is safe here: reaching this answer already required the correct
	// password, so it reveals nothing the caller had not already proven.
	assert.Equal(t, apperr.KindForbidden, apperr.KindOf(err))
}

func TestLogin_RejectsSuspendedUser(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)
	userID := f.signupAndVerify(t, "suspended@example.com")

	_, err := f.db.Exec(ctx,
		`UPDATE app_user SET status = 'suspended' WHERE id = $1`, string(userID))
	require.NoError(t, err)

	_, _, err = f.svc.Login(ctx, LoginParams{
		Email: "suspended@example.com", Password: goodPassword,
	})
	assert.ErrorIs(t, err, ErrUserSuspended)
}

// TestLogin_UnknownAndWrongPasswordAreIndistinguishable is the other half of
// the enumeration defence: the two failures a prober can reach must look
// identical.
func TestLogin_UnknownAndWrongPasswordAreIndistinguishable(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)
	f.signupAndVerify(t, "real@example.com")

	_, _, unknownErr := f.svc.Login(ctx, LoginParams{
		Email: "nobody@example.com", Password: goodPassword, IP: "203.0.113.1",
	})
	_, _, wrongErr := f.svc.Login(ctx, LoginParams{
		Email: "real@example.com", Password: "the wrong passphrase", IP: "203.0.113.2",
	})

	require.Error(t, unknownErr)
	require.Error(t, wrongErr)
	assert.Equal(t, unknownErr.Error(), wrongErr.Error(),
		"an unknown address and a wrong password must be indistinguishable")
	assert.ErrorIs(t, unknownErr, ErrCredentialsInvalid)
	assert.ErrorIs(t, wrongErr, ErrCredentialsInvalid)
}

// TestLogin_SuspendedUserIsNotRevealedByWrongPassword pins the order of the
// checks.
//
// Answering "suspended" to anyone who types the address would confirm that the
// address is real, which is exactly what the generic credential error exists
// to prevent. The status is only safe to disclose once the caller has proven
// they hold the password.
func TestLogin_SuspendedUserIsNotRevealedByWrongPassword(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)
	userID := f.signupAndVerify(t, "hidden@example.com")

	_, err := f.db.Exec(ctx,
		`UPDATE app_user SET status = 'suspended' WHERE id = $1`, string(userID))
	require.NoError(t, err)

	// Wrong password against a suspended account: indistinguishable from any
	// other bad guess.
	_, _, err = f.svc.Login(ctx, LoginParams{
		Email: "hidden@example.com", Password: "the wrong passphrase",
	})
	assert.ErrorIs(t, err, ErrCredentialsInvalid,
		"a probe must not learn that this address exists and is suspended")
	assert.NotErrorIs(t, err, ErrUserSuspended)

	// The correct password does get the specific answer, because at that point
	// there is nothing left to give away.
	_, _, err = f.svc.Login(ctx, LoginParams{
		Email: "hidden@example.com", Password: goodPassword,
	})
	assert.ErrorIs(t, err, ErrUserSuspended)
}

// TestFailedLoginIsAudited covers the attempts that matter most during a
// credential-stuffing run, including those against addresses nobody owns.
func TestFailedLoginIsAudited(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)
	f.signupAndVerify(t, "audited@example.com")

	_, _, err := f.svc.Login(ctx, LoginParams{
		Email: "audited@example.com", Password: "wrong", IP: "203.0.113.5",
	})
	require.Error(t, err)
	assert.Equal(t, 1, f.authEventCount(t, "audited@example.com", EventLoginFailed))

	// An attempt against an address that does not exist must still be
	// recorded, with no user attached.
	_, _, err = f.svc.Login(ctx, LoginParams{
		Email: "ghost@example.com", Password: "wrong", IP: "203.0.113.5",
	})
	require.Error(t, err)

	var userID *string
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT user_id FROM auth_event WHERE email = $1 AND type = $2`,
		"ghost@example.com", string(EventLoginFailed)).Scan(&userID))
	assert.Nil(t, userID, "an attempt on an unknown address has no user to attribute")
}

func TestLogin_ThrottlesRepeatedFailures(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)
	f.signupAndVerify(t, "throttled@example.com")

	params := LoginParams{
		Email: "throttled@example.com", Password: "wrong", IP: "203.0.113.7",
	}

	// The first few failures are free: people mistype passwords.
	for i := 0; i < throttleFreeAttempts; i++ {
		_, _, err := f.svc.Login(ctx, params)
		require.ErrorIs(t, err, ErrCredentialsInvalid,
			"attempt %d should not be throttled yet", i+1)
	}

	// The next one earns a penalty, and the one after that is refused before
	// the password is even checked.
	_, _, err := f.svc.Login(ctx, params)
	require.Error(t, err)

	_, _, err = f.svc.Login(ctx, params)
	var throttled *ThrottledError
	require.True(t, errors.As(err, &throttled), "further attempts must be throttled")
	assert.Greater(t, throttled.RetryAfter, time.Duration(0))
	assert.Equal(t, apperr.KindRateLimited, apperr.KindOf(err),
		"a throttled attempt must classify as rate limited, for a Retry-After header")
}

func TestLogin_ThrottleClearsOnSuccess(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)
	f.signupAndVerify(t, "recovers@example.com")

	// Some fumbling, but not enough to earn a penalty.
	for i := 0; i < throttleFreeAttempts-1; i++ {
		_, _, err := f.svc.Login(ctx, LoginParams{
			Email: "recovers@example.com", Password: "wrong", IP: "203.0.113.8",
		})
		require.Error(t, err)
	}

	_, _, err := f.svc.Login(ctx, LoginParams{
		Email: "recovers@example.com", Password: goodPassword, IP: "203.0.113.8",
	})
	require.NoError(t, err)

	// The slate is clean, so the earlier fumbling cannot combine with a future
	// mistake to lock someone out.
	for i := 0; i < throttleFreeAttempts; i++ {
		_, _, err := f.svc.Login(ctx, LoginParams{
			Email: "recovers@example.com", Password: "wrong", IP: "203.0.113.8",
		})
		require.ErrorIs(t, err, ErrCredentialsInvalid,
			"a success must have reset the counter")
	}
}

// TestSessionsDieOnPasswordChange proves one column signs every device out,
// with no delete that could miss a row.
func TestSessionsDieOnPasswordChange(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)
	f.signupAndVerify(t, "rotates@example.com")

	// Two devices, both signed in.
	first, _, err := f.svc.Login(ctx, LoginParams{
		Email: "rotates@example.com", Password: goodPassword, IP: "203.0.113.1",
	})
	require.NoError(t, err)
	second, _, err := f.svc.Login(ctx, LoginParams{
		Email: "rotates@example.com", Password: goodPassword, IP: "203.0.113.2",
	})
	require.NoError(t, err)

	for _, token := range []crypto.Token{first, second} {
		_, err := f.svc.ValidateSession(ctx, token.Plaintext())
		require.NoError(t, err, "both sessions must start out working")
	}

	require.NoError(t, f.svc.RequestPasswordReset(ctx, "rotates@example.com", "", ""))
	resetToken := f.mailer.resets["rotates@example.com"]
	require.NotEmpty(t, resetToken)

	const newPassword = "an entirely different passphrase"
	require.NoError(t, f.svc.ResetPassword(ctx, resetToken, newPassword, "", ""))

	// Every device is signed out, including the one that did not ask.
	for i, token := range []crypto.Token{first, second} {
		_, err := f.svc.ValidateSession(ctx, token.Plaintext())
		assert.ErrorIs(t, err, ErrSessionInvalid,
			"session %d must not survive a password change", i+1)
	}

	// And the new password is the one that works now.
	_, _, err = f.svc.Login(ctx, LoginParams{
		Email: "rotates@example.com", Password: goodPassword, IP: "203.0.113.3",
	})
	assert.ErrorIs(t, err, ErrCredentialsInvalid, "the old password must be dead")

	_, _, err = f.svc.Login(ctx, LoginParams{
		Email: "rotates@example.com", Password: newPassword, IP: "203.0.113.3",
	})
	assert.NoError(t, err)
}

func TestResetToken_IsSingleUse(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)
	f.signupAndVerify(t, "single@example.com")

	require.NoError(t, f.svc.RequestPasswordReset(ctx, "single@example.com", "", ""))
	token := f.mailer.resets["single@example.com"]
	require.NotEmpty(t, token)

	require.NoError(t, f.svc.ResetPassword(ctx, token, "the first new passphrase", "", ""))

	// A replayed link must not let anyone set a second password. This is the
	// one an attacker races the owner for.
	err := f.svc.ResetPassword(ctx, token, "an attacker's passphrase", "", "")
	assert.ErrorIs(t, err, ErrTokenInvalid)

	// The first reset is still the one in force.
	_, _, err = f.svc.Login(ctx, LoginParams{
		Email: "single@example.com", Password: "the first new passphrase",
	})
	assert.NoError(t, err)
}

// testClock is a time source a test can move forward, so expiry can be
// exercised without waiting for it.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock { return &testClock{now: time.Now().UTC()} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set moves the clock to an exact instant.
//
// Needed because some timestamps are written by Postgres rather than by this
// clock, and a fixture that mixes the two has to reconcile them explicitly.
func (c *testClock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

func TestResetToken_Expires(t *testing.T) {
	ctx := context.Background()

	db := fixtures.NewTestDB(t)
	redis := fixtures.NewTestCache(t)
	repo := NewPostgresRepository(db.DB)
	mailer := newCaptureMailer()
	clock := newTestClock()

	svc := NewService(repo, cheapHasher(), redis.Client, mailer, testSealer(t),
		NewCacheThrottle(redis.Client, redis.Client, nil), clock, nil, Options{
			ResetTokenLifetime: time.Hour,
		})

	f := &authFixture{db: db, cache: redis, svc: svc, repo: repo, mailer: mailer}
	f.signupAndVerify(t, "expired@example.com")

	require.NoError(t, svc.RequestPasswordReset(ctx, "expired@example.com", "", ""))
	token := mailer.resets["expired@example.com"]
	require.NotEmpty(t, token)

	// Still inside its hour, the link works. Checking this first means the
	// assertion below is about expiry and not about some other defect.
	require.NoError(t, svc.ResetPassword(ctx, token, "a brand new passphrase", "", ""))

	// A second link, this time left to go stale.
	require.NoError(t, svc.RequestPasswordReset(ctx, "expired@example.com", "", ""))
	stale := mailer.resets["expired@example.com"]
	require.NotEqual(t, token, stale)

	clock.Advance(time.Hour + time.Minute)

	err := svc.ResetPassword(ctx, stale, "an even newer passphrase", "", "")
	assert.ErrorIs(t, err, ErrTokenInvalid, "an expired link must not be usable")
}

// TestRequestPasswordReset_DoesNotRevealUnknownEmail is the third endpoint
// that would otherwise confirm which addresses are registered.
func TestRequestPasswordReset_DoesNotRevealUnknownEmail(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)

	err := f.svc.RequestPasswordReset(ctx, "nobody@example.com", "203.0.113.1", "")
	assert.NoError(t, err, "an unknown address must answer as a known one does")
	assert.Empty(t, f.mailer.resets, "and nothing may be sent")
}

func TestResetPassword_RejectsWeakPassword(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)
	f.signupAndVerify(t, "weak@example.com")

	require.NoError(t, f.svc.RequestPasswordReset(ctx, "weak@example.com", "", ""))
	token := f.mailer.resets["weak@example.com"]

	err := f.svc.ResetPassword(ctx, token, "short", "", "")
	require.Error(t, err)
	assert.Equal(t, apperr.KindInvalid, apperr.KindOf(err))

	// The token must survive a rejected attempt, or a typo would burn the link
	// and force the person to request another.
	require.NoError(t, f.svc.ResetPassword(ctx, token, "a properly long passphrase", "", ""))
}

func TestLogout_RevokesTheSession(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)
	f.signupAndVerify(t, "leaves@example.com")

	token, _, err := f.svc.Login(ctx, LoginParams{
		Email: "leaves@example.com", Password: goodPassword,
	})
	require.NoError(t, err)

	require.NoError(t, f.svc.Logout(ctx, token.Plaintext(), "", ""))

	_, err = f.svc.ValidateSession(ctx, token.Plaintext())
	assert.ErrorIs(t, err, ErrSessionInvalid)

	// Logging out twice, or with a token that means nothing, is normal client
	// behaviour rather than an error.
	assert.NoError(t, f.svc.Logout(ctx, token.Plaintext(), "", ""))
	assert.NoError(t, f.svc.Logout(ctx, "not-a-token", "", ""))
}

func TestLogin_UpgradesStaleHashWithoutSigningTheUserOut(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)
	f.signupAndVerify(t, "upgrade@example.com")

	// Rewrite the stored hash as one produced under weaker settings.
	weak := crypto.NewHasher(crypto.Params{MemoryKiB: 8, Iterations: 1, Threads: 1, KeyLength: 16})
	weakHash, err := weak.Hash(goodPassword)
	require.NoError(t, err)
	_, err = f.db.Exec(ctx,
		`UPDATE app_user SET password_hash = $2 WHERE email = $1`,
		"upgrade@example.com", weakHash)
	require.NoError(t, err)

	before, err := f.repo.UserByEmail(ctx, "upgrade@example.com")
	require.NoError(t, err)

	_, _, err = f.svc.Login(ctx, LoginParams{
		Email: "upgrade@example.com", Password: goodPassword,
	})
	require.NoError(t, err, "an old hash must still let the person in")

	after, err := f.repo.UserByEmail(ctx, "upgrade@example.com")
	require.NoError(t, err)
	assert.NotEqual(t, weakHash, after.PasswordHash, "the hash should have been upgraded")
	assert.Equal(t, before.PasswordChangedAt, after.PasswordChangedAt,
		"an upgrade is not a password change and must not sign other devices out")
}
