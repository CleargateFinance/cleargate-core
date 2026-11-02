//go:build integration

package account

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/crypto"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/mail"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

// accountFixture is a service wired to a throwaway Postgres and Redis, plus a
// user to open sessions for.
type accountFixture struct {
	db    *fixtures.TestDB
	cache *fixtures.TestCache
	svc   *Service
	repo  *countingRepo
	user  id.UserID
}

// countingRepo records how many times the database was consulted, so a test
// can prove a cache hit avoided it.
//
// The Repository interface is embedded rather than reimplemented, so only the
// one method under observation is overridden and the rest pass straight
// through to the real store.
type countingRepo struct {
	Repository
	lookups atomic.Int64
}

func (c *countingRepo) ActiveSessionByDigest(ctx context.Context, digest []byte) (Session, error) {
	c.lookups.Add(1)
	return c.Repository.ActiveSessionByDigest(ctx, digest)
}

// failingCache satisfies SessionCache and fails every call, standing in for
// Redis being down, unreachable or misbehaving.
type failingCache struct{ err error }

func (f failingCache) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, f.err
}
func (f failingCache) Set(context.Context, string, []byte, time.Duration) error { return f.err }
func (f failingCache) Delete(context.Context, string) error                     { return f.err }

// cheapHasher keeps sessions tests fast. Password cost is asserted in the
// crypto package's own tests, not here.
func cheapHasher() *crypto.Hasher {
	return crypto.NewHasher(crypto.Params{MemoryKiB: 8, Iterations: 1, Threads: 1})
}

// testSealer builds a real keyring over a throwaway key rather than a fake.
//
// AES costs nothing, so there is no speed argument for a stub, and using the
// real implementation means these tests exercise the encoding that will
// actually be written to the database.
func testSealer(t *testing.T) SecretSealer {
	t.Helper()

	key := make([]byte, crypto.MasterKeySize)
	_, err := rand.Read(key)
	require.NoError(t, err)

	ring, err := crypto.NewKeyring(map[byte][]byte{1: key}, 1)
	require.NoError(t, err)
	return ring
}

func newAccountFixture(t *testing.T) *accountFixture {
	t.Helper()
	return newAccountFixtureWithCache(t, nil)
}

// newAccountFixtureWithCache builds the fixture, optionally substituting the
// session cache so a test can simulate Redis failing.
func newAccountFixtureWithCache(t *testing.T, override SessionCache) *accountFixture {
	t.Helper()
	ctx := context.Background()

	db := fixtures.NewTestDB(t)
	redis := fixtures.NewTestCache(t)

	repo := &countingRepo{Repository: NewPostgresRepository(db.DB)}

	var sessionCache SessionCache = redis.Client
	if override != nil {
		sessionCache = override
	}

	svc := NewService(repo, cheapHasher(), sessionCache,
		mail.DiscardMailer{}, testSealer(t),
		NewCacheThrottle(sessionCache, redis.Client, nil), nil, nil, Options{})

	f := &accountFixture{db: db, cache: redis, svc: svc, repo: repo}
	f.user = f.createUser(ctx, t, "founder@example.com")
	return f
}

// createUser inserts the principal and login a session needs to hang off.
//
// This writes the row directly instead of going through Signup, so it does not
// get the application-clock timestamp Signup supplies. password_changed_at is
// therefore backdated by hand: a session is valid only when
// issued_at >= password_changed_at, and with the row written by Postgres and
// the session stamped by the application, the two are milliseconds apart and
// the comparison would turn on however far a container's clock has drifted
// from its host's.
func (f *accountFixture) createUser(ctx context.Context, t *testing.T, email string) id.UserID {
	t.Helper()

	hash, err := f.svc.HashPassword("a perfectly fine password")
	require.NoError(t, err)

	var principalID string
	require.NoError(t, f.db.QueryRow(ctx,
		`INSERT INTO principal (kind) VALUES ('person') RETURNING id`).Scan(&principalID))

	var userID string
	require.NoError(t, f.db.QueryRow(ctx, `
		INSERT INTO app_user (principal_id, email, password_hash, email_verified_at,
		                      password_changed_at)
		VALUES ($1, $2, $3, now(), now() - interval '1 minute')
		RETURNING id`, principalID, email, hash).Scan(&userID))

	return id.UserID(userID)
}

// rawCacheEntry reads what is actually stored in Redis for a token.
func (f *accountFixture) rawCacheEntry(t *testing.T, token crypto.Token) ([]byte, bool) {
	t.Helper()
	value, found, err := f.cache.Get(context.Background(), sessionCacheKey(token.Digest()))
	require.NoError(t, err)
	return value, found
}

func TestIssueSession_StoresDigestAndReturnsToken(t *testing.T) {
	ctx := context.Background()
	f := newAccountFixture(t)

	token, session, err := f.svc.IssueSession(ctx, NewSessionParams{
		UserID: f.user, IP: "203.0.113.7", UserAgent: "console/1.0",
	})
	require.NoError(t, err)

	assert.NotEmpty(t, session.ID)
	assert.Equal(t, f.user, session.UserID)
	assert.NotEmpty(t, token.Plaintext())
	assert.True(t, session.ExpiresAt.After(session.IssuedAt))
	assert.Nil(t, session.MFASatisfiedAt, "no second factor was presented")
}

func TestIssueSession_RecordsMFAWhenPresented(t *testing.T) {
	ctx := context.Background()
	f := newAccountFixture(t)

	_, session, err := f.svc.IssueSession(ctx, NewSessionParams{
		UserID: f.user, MFASatisfied: true,
	})
	require.NoError(t, err)

	require.NotNil(t, session.MFASatisfiedAt)
	assert.True(t, session.MFARecent(time.Now(), DefaultStepUpWindow),
		"a factor presented at login must open the step-up window")
}

// TestSessionLookup_NeverStoresPlaintext is the property that makes a leak of
// the session table worthless to an attacker.
func TestSessionLookup_NeverStoresPlaintext(t *testing.T) {
	ctx := context.Background()
	f := newAccountFixture(t)

	token, _, err := f.svc.IssueSession(ctx, NewSessionParams{UserID: f.user})
	require.NoError(t, err)

	var stored []byte
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT token_hash FROM user_session WHERE user_id = $1`,
		string(f.user)).Scan(&stored))

	assert.Equal(t, crypto.DigestToken(token.Plaintext()), stored,
		"the column must hold the digest of the token")
	assert.NotEqual(t, token.Plaintext(), string(stored),
		"the column must never hold the token itself")

	// And the token must still be usable, or the digest is the wrong one.
	session, err := f.svc.ValidateSession(ctx, token.Plaintext())
	require.NoError(t, err)
	assert.Equal(t, f.user, session.UserID)
}

func TestValidateSession_RejectsUnknownToken(t *testing.T) {
	ctx := context.Background()
	f := newAccountFixture(t)

	_, err := f.svc.ValidateSession(ctx, "not-a-real-token")
	assert.ErrorIs(t, err, ErrSessionInvalid)

	_, err = f.svc.ValidateSession(ctx, "")
	assert.ErrorIs(t, err, ErrSessionInvalid)

	// An unknown token must leave nothing behind, so hammering the endpoint
	// cannot fill the cache with entries that were never valid.
	value, found, err := f.cache.Get(ctx,
		sessionCacheKey(crypto.DigestToken("not-a-real-token")))
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, value)
}

// TestSessionCache_HitAvoidsPostgres proves the fast path is actually fast:
// the second validation must not reach the database at all.
func TestSessionCache_HitAvoidsPostgres(t *testing.T) {
	ctx := context.Background()
	f := newAccountFixture(t)

	token, _, err := f.svc.IssueSession(ctx, NewSessionParams{UserID: f.user})
	require.NoError(t, err)

	// The first validation is a miss: it reads Postgres and populates Redis.
	_, err = f.svc.ValidateSession(ctx, token.Plaintext())
	require.NoError(t, err)
	require.Equal(t, int64(1), f.repo.lookups.Load())

	_, found := f.rawCacheEntry(t, token)
	require.True(t, found, "a successful lookup must populate the cache")

	// Every validation after that is served from Redis.
	for i := 0; i < 5; i++ {
		session, err := f.svc.ValidateSession(ctx, token.Plaintext())
		require.NoError(t, err)
		assert.Equal(t, f.user, session.UserID)
	}

	assert.Equal(t, int64(1), f.repo.lookups.Load(),
		"a cache hit must not consult the database")
}

// TestRevokeDeletesCacheEntry is what makes revocation instant rather than
// eventual, which is the whole reason these are opaque tokens and not JWTs.
func TestRevokeDeletesCacheEntry(t *testing.T) {
	ctx := context.Background()
	f := newAccountFixture(t)

	token, _, err := f.svc.IssueSession(ctx, NewSessionParams{UserID: f.user})
	require.NoError(t, err)

	_, err = f.svc.ValidateSession(ctx, token.Plaintext())
	require.NoError(t, err)
	_, found := f.rawCacheEntry(t, token)
	require.True(t, found)

	require.NoError(t, f.svc.RevokeSession(ctx, token.Plaintext()))

	_, found = f.rawCacheEntry(t, token)
	assert.False(t, found, "revocation must remove the cached entry, not wait for its TTL")

	_, err = f.svc.ValidateSession(ctx, token.Plaintext())
	assert.ErrorIs(t, err, ErrSessionInvalid,
		"a revoked session must stop working immediately")
}

func TestRevokeSession_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newAccountFixture(t)

	token, _, err := f.svc.IssueSession(ctx, NewSessionParams{UserID: f.user})
	require.NoError(t, err)

	require.NoError(t, f.svc.RevokeSession(ctx, token.Plaintext()))
	require.NoError(t, f.svc.RevokeSession(ctx, token.Plaintext()),
		"signing out twice is normal client behaviour, not a failure")
}

// TestRedisDownFallsBackToPostgres is the property that keeps a cache blip
// from becoming an outage. Every cache failure degrades to the path that was
// correct before the cache existed.
func TestRedisDownFallsBackToPostgres(t *testing.T) {
	ctx := context.Background()
	broken := failingCache{err: errors.New("redis is down")}
	f := newAccountFixtureWithCache(t, broken)

	token, _, err := f.svc.IssueSession(ctx, NewSessionParams{UserID: f.user})
	require.NoError(t, err)

	// Reads still work, because a cache error is treated as a miss.
	for i := 0; i < 3; i++ {
		session, err := f.svc.ValidateSession(ctx, token.Plaintext())
		require.NoError(t, err, "a failing cache must not fail the request")
		assert.Equal(t, f.user, session.UserID)
	}
	assert.Equal(t, int64(3), f.repo.lookups.Load(),
		"every validation must have fallen through to the database")

	// Revocation still works, because Postgres is written before the cache is
	// touched, and the cache failure cannot undo a committed row.
	require.NoError(t, f.svc.RevokeSession(ctx, token.Plaintext()),
		"a failing cache must not fail a sign-out the database committed")

	_, err = f.svc.ValidateSession(ctx, token.Plaintext())
	assert.ErrorIs(t, err, ErrSessionInvalid)
}

// TestCacheEntryNeverOutlivesSession checks the TTL is the shorter of the
// cache window and what remains of the session.
func TestCacheEntryNeverOutlivesSession(t *testing.T) {
	ctx := context.Background()

	t.Run("capped by the cache window for a long session", func(t *testing.T) {
		f := newAccountFixture(t)

		token, _, err := f.svc.IssueSession(ctx, NewSessionParams{UserID: f.user})
		require.NoError(t, err)
		_, err = f.svc.ValidateSession(ctx, token.Plaintext())
		require.NoError(t, err)

		ttl, err := f.cache.TTL(ctx, sessionCacheKey(token.Digest()))
		require.NoError(t, err)

		assert.Greater(t, ttl, time.Duration(0))
		assert.LessOrEqual(t, ttl, DefaultCacheWindow,
			"a twelve hour session must still only be cached for the window")
	})

	t.Run("capped by the session for a short one", func(t *testing.T) {
		db := fixtures.NewTestDB(t)
		redis := fixtures.NewTestCache(t)
		repo := &countingRepo{Repository: NewPostgresRepository(db.DB)}

		// A session shorter than the cache window: the entry must expire with
		// the session, not outlive it.
		svc := NewService(repo, cheapHasher(), redis.Client,
			mail.DiscardMailer{}, testSealer(t),
			NewCacheThrottle(redis.Client, redis.Client, nil), nil, nil, Options{
				SessionLifetime: 10 * time.Second,
			})

		f := &accountFixture{db: db, cache: redis, svc: svc, repo: repo}
		f.user = f.createUser(ctx, t, "shortlived@example.com")

		token, _, err := svc.IssueSession(ctx, NewSessionParams{UserID: f.user})
		require.NoError(t, err)
		_, err = svc.ValidateSession(ctx, token.Plaintext())
		require.NoError(t, err)

		ttl, err := redis.TTL(ctx, sessionCacheKey(token.Digest()))
		require.NoError(t, err)

		assert.Greater(t, ttl, time.Duration(0))
		assert.LessOrEqual(t, ttl, 10*time.Second,
			"the entry must not survive the session it describes")
	})
}

// TestCachedSessionCarriesNoRoles guards the rule that the cache holds only
// what deleting its key already invalidates.
//
// A membership role changes on its own schedule, so cached beside the session
// it would outlive its own revocation: demoting an admin would not take effect
// until that person happened to sign out.
func TestCachedSessionCarriesNoRoles(t *testing.T) {
	ctx := context.Background()
	f := newAccountFixture(t)

	token, _, err := f.svc.IssueSession(ctx, NewSessionParams{
		UserID: f.user, MFASatisfied: true,
	})
	require.NoError(t, err)
	_, err = f.svc.ValidateSession(ctx, token.Plaintext())
	require.NoError(t, err)

	raw, found := f.rawCacheEntry(t, token)
	require.True(t, found)

	var entry map[string]any
	require.NoError(t, json.Unmarshal(raw, &entry))

	for _, forbidden := range []string{"role", "roles", "permissions", "memberships", "account_id"} {
		assert.NotContains(t, entry, forbidden,
			"authorisation data must be read from Postgres, never cached beside the session")
	}

	// What it does carry is identity and lifetime, and nothing more.
	assert.Equal(t, string(f.user), entry["user_id"])
	assert.Contains(t, entry, "expires_at")
	assert.Contains(t, entry, "mfa_satisfied_at")
}

// TestCachedSessionKeyIsDigestNotToken checks that a dump of Redis is no more
// useful than a dump of the sessions table.
func TestCachedSessionKeyIsDigestNotToken(t *testing.T) {
	ctx := context.Background()
	f := newAccountFixture(t)

	token, _, err := f.svc.IssueSession(ctx, NewSessionParams{UserID: f.user})
	require.NoError(t, err)
	_, err = f.svc.ValidateSession(ctx, token.Plaintext())
	require.NoError(t, err)

	key := sessionCacheKey(token.Digest())
	assert.NotContains(t, key, token.Plaintext(), "the key must not embed the token")
	assert.Equal(t, sessionKeyPrefix+hex.EncodeToString(token.Digest()), key)

	raw, found := f.rawCacheEntry(t, token)
	require.True(t, found)
	assert.NotContains(t, string(raw), token.Plaintext(),
		"the cached value must not contain the token either")
}

// TestValidateSession_RejectsSuspendedUser proves suspension ends sessions
// without a separate sweep, because the condition lives in the lookup.
func TestValidateSession_RejectsSuspendedUser(t *testing.T) {
	ctx := context.Background()
	f := newAccountFixture(t)

	token, _, err := f.svc.IssueSession(ctx, NewSessionParams{UserID: f.user})
	require.NoError(t, err)

	_, err = f.db.Exec(ctx,
		`UPDATE app_user SET status = 'suspended' WHERE id = $1`, string(f.user))
	require.NoError(t, err)

	// Cleared so this exercises the database path rather than a warm entry;
	// the cached path accepts staleness bounded by the cache window.
	require.NoError(t, f.cache.Delete(ctx, sessionCacheKey(token.Digest())))

	_, err = f.svc.ValidateSession(ctx, token.Plaintext())
	assert.ErrorIs(t, err, ErrSessionInvalid)
}

func TestPasswordRoundTripThroughTheDatabase(t *testing.T) {
	ctx := context.Background()
	f := newAccountFixture(t)

	var stored string
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT password_hash FROM app_user WHERE id = $1`,
		string(f.user)).Scan(&stored))

	assert.Contains(t, stored, "$argon2id$", "the column must hold a PHC string")
	assert.NotContains(t, stored, "a perfectly fine password")

	ok, err := f.svc.VerifyPassword(stored, "a perfectly fine password")
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = f.svc.VerifyPassword(stored, "the wrong password")
	require.NoError(t, err)
	assert.False(t, ok)
}
