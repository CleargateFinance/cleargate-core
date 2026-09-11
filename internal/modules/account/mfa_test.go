//go:build integration

package account

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/crypto"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

// mfaFixture is a service on a throwaway Postgres and Redis, with a clock the
// test controls so the step-up window can be crossed without waiting.
type mfaFixture struct {
	db     *fixtures.TestDB
	cache  *fixtures.TestCache
	svc    *Service
	repo   Repository
	mailer *captureMailer
	clock  *testClock

	user  id.UserID
	email string
}

func newMFAFixture(t *testing.T) *mfaFixture {
	t.Helper()

	db := fixtures.NewTestDB(t)
	redis := fixtures.NewTestCache(t)
	repo := NewPostgresRepository(db.DB)
	mailer := newCaptureMailer()
	clock := newTestClock()

	svc := NewService(repo, cheapHasher(), redis.Client, mailer, testSealer(t),
		NewCacheThrottle(redis.Client, redis.Client, nil), clock, nil, Options{})

	f := &mfaFixture{
		db: db, cache: redis, svc: svc, repo: repo,
		mailer: mailer, clock: clock, email: "founder@example.com",
	}

	auth := &authFixture{db: db, cache: redis, svc: svc, repo: repo, mailer: mailer}
	f.user = auth.signupAndVerify(t, f.email)

	// Anchor the controllable clock to the database's own, so that the several
	// tests below which advance it past a TOTP period or a step-up window are
	// moving relative to a known instant rather than to wherever the host's
	// clock happened to sit when this fixture was built.
	var dbNow time.Time
	require.NoError(t, db.QueryRow(context.Background(), `SELECT now()`).Scan(&dbNow))
	clock.Set(dbNow)

	return f
}

// session opens a login session for the fixture's user.
func (f *mfaFixture) session(t *testing.T) (string, Session) {
	t.Helper()

	token, session, err := f.svc.IssueSession(context.Background(), NewSessionParams{
		UserID: f.user, IP: "203.0.113.1",
	})
	require.NoError(t, err)
	return token.Plaintext(), session
}

// codeFor produces what the person's authenticator app would show right now.
func (f *mfaFixture) codeFor(t *testing.T, secret string) string {
	t.Helper()
	return f.codeForStep(t, secret, crypto.TOTPStep(f.clock.Now()))
}

func (f *mfaFixture) codeForStep(t *testing.T, secret string, step int64) string {
	t.Helper()
	code, err := hotp.GenerateCodeCustom(secret, uint64(step), hotp.ValidateOpts{
		Digits:    crypto.TOTPDigits,
		Algorithm: otp.AlgorithmSHA1,
	})
	require.NoError(t, err)
	return code
}

// enrol runs the whole enrolment path and returns the secret and the recovery
// codes, so a test can start from somebody who already holds a second factor.
//
// The clock is moved on by one period afterwards, because confirming spends the
// step it was confirmed with. That is the replay guard doing its job rather than
// an inconvenience to work around: the person's app has rolled to a new code by
// the time they use their factor for anything, and a test that reused the
// confirmation code would be asserting that replay is possible.
func (f *mfaFixture) enrol(t *testing.T) (secret string, recoveryCodes []string) {
	t.Helper()
	ctx := context.Background()

	enrolment, err := f.svc.BeginTOTPEnrolment(ctx, f.user, f.email)
	require.NoError(t, err)

	codes, err := f.svc.ConfirmTOTPEnrolment(ctx, f.user,
		f.codeFor(t, enrolment.Secret), "203.0.113.1", "test")
	require.NoError(t, err)

	f.clock.Advance(time.Duration(crypto.TOTPPeriod) * time.Second)

	return enrolment.Secret, codes
}

// TestTOTPEnrolment_RequiresConfirmingCode is what stops a half-finished
// enrolment from locking somebody out.
//
// A secret that was generated but never proven to produce matching codes must
// protect nothing. Otherwise a mistyped secret, a camera that never focused, or
// a browser tab closed halfway would leave an account demanding a code that
// nothing on earth can produce.
func TestTOTPEnrolment_RequiresConfirmingCode(t *testing.T) {
	ctx := context.Background()
	f := newMFAFixture(t)

	enrolment, err := f.svc.BeginTOTPEnrolment(ctx, f.user, f.email)
	require.NoError(t, err)
	require.NotEmpty(t, enrolment.Secret)

	// A pending factor exists, but it counts for nothing yet.
	enrolled, err := f.repo.HasConfirmedMFA(ctx, f.user)
	require.NoError(t, err)
	assert.False(t, enrolled, "an unconfirmed factor must not satisfy enrolment")

	_, session := f.session(t)
	err = f.svc.RequireStepUp(ctx, session)
	var stepUp *StepUpError
	require.ErrorAs(t, err, &stepUp)
	assert.Equal(t, ReasonMFARequired, stepUp.Reason)

	// A wrong code must not confirm it either.
	_, err = f.svc.ConfirmTOTPEnrolment(ctx, f.user, "000000", "203.0.113.1", "test")
	assert.ErrorIs(t, err, ErrMFACodeInvalid)

	enrolled, err = f.repo.HasConfirmedMFA(ctx, f.user)
	require.NoError(t, err)
	assert.False(t, enrolled)

	// The right code completes it, and only then does the factor count.
	codes, err := f.svc.ConfirmTOTPEnrolment(ctx, f.user,
		f.codeFor(t, enrolment.Secret), "203.0.113.1", "test")
	require.NoError(t, err)
	assert.Len(t, codes, RecoveryCodeCount, "confirming issues the recovery codes")

	enrolled, err = f.repo.HasConfirmedMFA(ctx, f.user)
	require.NoError(t, err)
	assert.True(t, enrolled)
}

// TestTOTPSecret_IsEncryptedAtRest checks the column holds an envelope rather
// than the secret.
//
// A shared secret has to be readable by the server to verify a code, so it
// cannot be hashed. That makes this column the one place where a database
// disclosure would otherwise hand over everybody's second factor directly.
func TestTOTPSecret_IsEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	f := newMFAFixture(t)

	secret, _ := f.enrol(t)

	var stored []byte
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT secret_enc FROM user_mfa_factor WHERE user_id = $1`,
		string(f.user)).Scan(&stored))

	assert.False(t, bytes.Contains(stored, []byte(secret)),
		"the stored value must not contain the shared secret")

	// And it must still verify, or the encryption is protecting a value the
	// service can no longer use.
	token, _ := f.session(t)
	_, err := f.svc.SatisfyTOTP(ctx, token, f.codeFor(t, secret), "203.0.113.1", "test")
	assert.NoError(t, err)
}

// TestTOTPCode_CannotBeReplayed is the property last_used_step exists for.
//
// A code stays valid for its whole window, so somebody who observes one — over
// a shoulder, through a phishing proxy, in a log — can present it again until
// the window closes. Recording the highest step already accepted makes each
// code usable exactly once.
func TestTOTPCode_CannotBeReplayed(t *testing.T) {
	ctx := context.Background()
	f := newMFAFixture(t)

	secret, _ := f.enrol(t)
	code := f.codeFor(t, secret)

	first, _ := f.session(t)
	_, err := f.svc.SatisfyTOTP(ctx, first, code, "203.0.113.1", "test")
	require.NoError(t, err)

	// The same code, still inside its window, on a fresh session. A different
	// session is used deliberately: the guard has to live on the factor, not on
	// whatever session happened to spend the code.
	second, _ := f.session(t)
	_, err = f.svc.SatisfyTOTP(ctx, second, code, "203.0.113.1", "test")
	assert.ErrorIs(t, err, ErrMFACodeInvalid,
		"a code must not be usable twice, even inside its own window")
}

// TestTOTPCode_EarlierStepIsRefusedAfterALaterOne covers the other half of the
// guard: the window reaches one step backwards, so spending step N must also
// burn step N-1 rather than leaving it open.
func TestTOTPCode_EarlierStepIsRefusedAfterALaterOne(t *testing.T) {
	ctx := context.Background()
	f := newMFAFixture(t)

	secret, _ := f.enrol(t)
	current := crypto.TOTPStep(f.clock.Now())

	token, _ := f.session(t)
	_, err := f.svc.SatisfyTOTP(ctx, token, f.codeForStep(t, secret, current), "203.0.113.1", "test")
	require.NoError(t, err)

	next, _ := f.session(t)
	_, err = f.svc.SatisfyTOTP(ctx, next, f.codeForStep(t, secret, current-1), "203.0.113.1", "test")
	assert.ErrorIs(t, err, ErrMFACodeInvalid,
		"a step below the highest already spent must be refused")
}

// TestRecoveryCode_IsSingleUse keeps a list found on paper worth exactly one
// login rather than permanent access.
func TestRecoveryCode_IsSingleUse(t *testing.T) {
	ctx := context.Background()
	f := newMFAFixture(t)

	_, codes := f.enrol(t)
	require.Len(t, codes, RecoveryCodeCount)

	token, _ := f.session(t)
	session, err := f.svc.SatisfyRecoveryCode(ctx, token, codes[0], "203.0.113.1", "test")
	require.NoError(t, err)
	require.NotNil(t, session.MFASatisfiedAt, "a recovery code opens the window too")

	// The same code again must fail, on a fresh session so nothing about the
	// first one can be carrying the result.
	second, _ := f.session(t)
	_, err = f.svc.SatisfyRecoveryCode(ctx, second, codes[0], "203.0.113.1", "test")
	assert.ErrorIs(t, err, ErrMFACodeInvalid)

	// A different code from the same list still works, so spending one does not
	// invalidate the rest.
	third, _ := f.session(t)
	_, err = f.svc.SatisfyRecoveryCode(ctx, third, codes[1], "203.0.113.1", "test")
	assert.NoError(t, err)
}

// TestRecoveryCode_AcceptsWhatAPersonWouldType covers transcription from paper,
// which is the only way these are ever entered.
func TestRecoveryCode_AcceptsWhatAPersonWouldType(t *testing.T) {
	ctx := context.Background()
	f := newMFAFixture(t)

	_, codes := f.enrol(t)

	// Lowercase, with the separator dropped and stray spaces, exactly as
	// somebody reading their own handwriting would produce.
	typed := " " + NormalizeRecoveryCode(codes[0])
	typed = " " + typed[1:5] + " " + typed[5:]

	token, _ := f.session(t)
	_, err := f.svc.SatisfyRecoveryCode(ctx, token, typed, "203.0.113.1", "test")
	assert.NoError(t, err)
}

// TestRecoveryCodes_ReplacedOnReEnrolment stops a list printed against an old
// factor from outliving the factor it belonged to.
func TestRecoveryCodes_ReplacedOnReEnrolment(t *testing.T) {
	ctx := context.Background()
	f := newMFAFixture(t)

	_, first := f.enrol(t)

	// Clear the confirmed factor the way a removal would, then enrol again.
	_, err := f.db.Exec(ctx,
		`UPDATE user_mfa_factor SET revoked_at = now() WHERE user_id = $1`, string(f.user))
	require.NoError(t, err)

	_, second := f.enrol(t)
	require.NotEqual(t, first, second)

	token, _ := f.session(t)
	_, err = f.svc.SatisfyRecoveryCode(ctx, token, first[0], "203.0.113.1", "test")
	assert.ErrorIs(t, err, ErrMFACodeInvalid,
		"codes from a retired factor must not still open the account")
}

// TestEnrolment_CannotSilentlyReplaceAConfirmedFactor is what stops an attacker
// who has only a session from swapping the second factor for their own.
func TestEnrolment_CannotSilentlyReplaceAConfirmedFactor(t *testing.T) {
	ctx := context.Background()
	f := newMFAFixture(t)

	f.enrol(t)

	_, err := f.svc.BeginTOTPEnrolment(ctx, f.user, f.email)
	assert.ErrorIs(t, err, ErrMFAAlreadyEnrolled)
}

// TestEnrolment_CanBeRestartedBeforeConfirming covers the ordinary case of a QR
// code scanned on a device the person no longer has in front of them.
func TestEnrolment_CanBeRestartedBeforeConfirming(t *testing.T) {
	ctx := context.Background()
	f := newMFAFixture(t)

	abandoned, err := f.svc.BeginTOTPEnrolment(ctx, f.user, f.email)
	require.NoError(t, err)

	restarted, err := f.svc.BeginTOTPEnrolment(ctx, f.user, f.email)
	require.NoError(t, err)
	require.NotEqual(t, abandoned.Secret, restarted.Secret)

	// Only one pending factor survives, or "the pending factor" is ambiguous.
	var pending int
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT count(*) FROM user_mfa_factor WHERE user_id = $1 AND revoked_at IS NULL`,
		string(f.user)).Scan(&pending))
	assert.Equal(t, 1, pending)

	// The abandoned secret must not confirm anything.
	_, err = f.svc.ConfirmTOTPEnrolment(ctx, f.user,
		f.codeFor(t, abandoned.Secret), "203.0.113.1", "test")
	assert.ErrorIs(t, err, ErrMFACodeInvalid)

	_, err = f.svc.ConfirmTOTPEnrolment(ctx, f.user,
		f.codeFor(t, restarted.Secret), "203.0.113.1", "test")
	assert.NoError(t, err)
}

// TestStepUp_RejectedWithoutAnyFactor is the gate funding and invitations sit
// behind: a session alone is not enough to bring money in or add a person.
func TestStepUp_RejectedWithoutAnyFactor(t *testing.T) {
	ctx := context.Background()
	f := newMFAFixture(t)

	_, session := f.session(t)

	err := f.svc.RequireStepUp(ctx, session)
	var stepUp *StepUpError
	require.ErrorAs(t, err, &stepUp)
	assert.Equal(t, ReasonMFARequired, stepUp.Reason,
		"the console has to know to start an enrolment, not ask for a code")
}

// TestStepUp_ExpiresAfterWindow is why the session stores a moment rather than
// a boolean.
//
// A factor presented once at login must not authorise a funding request made
// from the same tab hours later, because by then the evidence that the person
// is still at the keyboard has gone stale.
func TestStepUp_ExpiresAfterWindow(t *testing.T) {
	ctx := context.Background()
	f := newMFAFixture(t)

	secret, _ := f.enrol(t)
	token, _ := f.session(t)

	session, err := f.svc.SatisfyTOTP(ctx, token, f.codeFor(t, secret), "203.0.113.1", "test")
	require.NoError(t, err)

	// Immediately afterwards the window is open.
	require.NoError(t, f.svc.RequireStepUp(ctx, session))

	// Just inside it, still open.
	f.clock.Advance(DefaultStepUpWindow - time.Minute)
	require.NoError(t, f.svc.RequireStepUp(ctx, session))

	// Past it, closed, and the reason says "re-enter a code" rather than
	// "enrol", because the factor itself is still perfectly good.
	f.clock.Advance(2 * time.Minute)
	err = f.svc.RequireStepUp(ctx, session)

	var stepUp *StepUpError
	require.ErrorAs(t, err, &stepUp)
	assert.Equal(t, ReasonStepUpRequired, stepUp.Reason)
}

// TestSatisfyTOTP_RefreshesTheWindowOnTheStoredSession checks the stamp reaches
// Postgres and the cached copy is dropped.
//
// A stale cache entry here would reject the very action the code was just
// entered for, which is the kind of bug that looks like the second factor
// simply not working.
func TestSatisfyTOTP_RefreshesTheWindowOnTheStoredSession(t *testing.T) {
	ctx := context.Background()
	f := newMFAFixture(t)

	secret, _ := f.enrol(t)
	token, _ := f.session(t)

	// Validate once first, so the session is definitely in the cache saying no
	// factor has been presented.
	cached, err := f.svc.ValidateSession(ctx, token)
	require.NoError(t, err)
	require.Nil(t, cached.MFASatisfiedAt)

	_, err = f.svc.SatisfyTOTP(ctx, token, f.codeFor(t, secret), "203.0.113.1", "test")
	require.NoError(t, err)

	// A fresh read, which is what a subsequent request would do, must see it.
	reloaded, err := f.svc.ValidateSession(ctx, token)
	require.NoError(t, err)
	require.NotNil(t, reloaded.MFASatisfiedAt,
		"the next request must see the factor that was just presented")
	assert.NoError(t, f.svc.RequireStepUp(ctx, reloaded))
}

// TestSatisfyTOTP_ThrottlesGuessing keeps a six-digit space out of reach of
// whoever holds only a stolen session cookie.
func TestSatisfyTOTP_ThrottlesGuessing(t *testing.T) {
	ctx := context.Background()
	f := newMFAFixture(t)

	f.enrol(t)
	token, _ := f.session(t)

	// Walk past the free attempts. Every one of these is a wrong code.
	var throttled *ThrottledError
	for range 20 {
		_, err := f.svc.SatisfyTOTP(ctx, token, "000000", "203.0.113.1", "test")
		if errors.As(err, &throttled) {
			break
		}
		require.ErrorIs(t, err, ErrMFACodeInvalid)
	}

	require.NotNil(t, throttled, "repeated wrong codes must start costing time")
	assert.Positive(t, throttled.RetryAfter)
}

// TestSatisfyTOTP_RequiresAConfirmedFactor keeps a pending enrolment from
// opening the step-up window.
func TestSatisfyTOTP_RequiresAConfirmedFactor(t *testing.T) {
	ctx := context.Background()
	f := newMFAFixture(t)

	enrolment, err := f.svc.BeginTOTPEnrolment(ctx, f.user, f.email)
	require.NoError(t, err)

	token, _ := f.session(t)
	_, err = f.svc.SatisfyTOTP(ctx, token, f.codeFor(t, enrolment.Secret), "203.0.113.1", "test")
	assert.ErrorIs(t, err, ErrMFANotEnrolled)
}

// TestRecoveryCodes_AreHashedAtRest keeps a disclosure of this table from
// yielding a working list.
func TestRecoveryCodes_AreHashedAtRest(t *testing.T) {
	ctx := context.Background()
	f := newMFAFixture(t)

	_, codes := f.enrol(t)

	rows, err := f.db.Query(ctx,
		`SELECT code_hash FROM user_recovery_code WHERE user_id = $1`, string(f.user))
	require.NoError(t, err)
	defer rows.Close()

	var stored []string
	for rows.Next() {
		var hash string
		require.NoError(t, rows.Scan(&hash))
		stored = append(stored, hash)
	}
	require.NoError(t, rows.Err())
	require.Len(t, stored, RecoveryCodeCount)

	for _, hash := range stored {
		assert.Contains(t, hash, "$argon2id$", "codes must be stored as argon2id hashes")
		for _, code := range codes {
			assert.NotContains(t, hash, code)
		}
	}
}
