package crypto

import (
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cheapParams keep the unit tests fast. Cost is the one thing these tests are
// not asserting, so paying the real 19 MiB per derivation would only make the
// suite slow. The tests that do care about cost state their own parameters.
func cheapParams() Params {
	return Params{
		MemoryKiB:  8,
		Iterations: 1,
		Threads:    1,
		SaltLength: 16,
		KeyLength:  32,
	}
}

func TestHashPassword_ProducesPHCString(t *testing.T) {
	// This test asserts the real defaults, since the encoded parameters are
	// the point of the format.
	h := NewHasher(DefaultParams())

	encoded, err := h.Hash("correct horse battery staple")
	require.NoError(t, err)

	// $argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>
	phc := regexp.MustCompile(
		`^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]+\$[A-Za-z0-9+/]+$`)
	assert.Regexp(t, phc, encoded,
		"the encoding must carry its own parameters, so cost can be raised later")

	// The salt and hash fields are unpadded base64. '=' appears elsewhere in
	// the string as part of v= and m=, so only the last two fields are checked.
	fields := strings.Split(encoded, "$")
	require.Len(t, fields, 6)
	assert.NotContains(t, fields[4], "=", "the salt field must be unpadded base64")
	assert.NotContains(t, fields[5], "=", "the hash field must be unpadded base64")
}

func TestHashPassword_SaltsEveryHash(t *testing.T) {
	h := NewHasher(cheapParams())

	first, err := h.Hash("same password")
	require.NoError(t, err)
	second, err := h.Hash("same password")
	require.NoError(t, err)

	// Identical passwords must not produce identical rows, or a leaked table
	// reveals which users share a password.
	assert.NotEqual(t, first, second, "each hash must draw a fresh salt")
}

func TestVerify_RejectsWrongPassword(t *testing.T) {
	h := NewHasher(cheapParams())

	encoded, err := h.Hash("the right one")
	require.NoError(t, err)

	ok, err := h.Verify(encoded, "the right one")
	require.NoError(t, err)
	assert.True(t, ok, "the correct password must verify")

	ok, err = h.Verify(encoded, "the wrong one")
	require.NoError(t, err)
	assert.False(t, ok, "a wrong password must not verify")

	// A near miss must fail exactly like a far one.
	ok, err = h.Verify(encoded, "the right on")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestVerify_RejectsMalformedHash(t *testing.T) {
	h := NewHasher(cheapParams())

	cases := map[string]string{
		"empty":              "",
		"not a phc string":   "just-some-text",
		"truncated":          "$argon2id$v=19$m=8,t=1,p=1$c29tZXNhbHQ",
		"unknown algorithm":  "$bcrypt$v=19$m=8,t=1,p=1$c29tZXNhbHQ$aGFzaA",
		"unknown version":    "$argon2id$v=16$m=8,t=1,p=1$c29tZXNhbHQ$aGFzaA",
		"bad base64 in salt": "$argon2id$v=19$m=8,t=1,p=1$!!!not-base64!!!$aGFzaA",
	}

	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			ok, err := h.Verify(encoded, "anything")
			assert.False(t, ok)
			assert.ErrorIs(t, err, ErrInvalidHash,
				"a hash we cannot parse must be an error, never a silent pass")
		})
	}
}

func TestVerify_RejectsOverlongPassword(t *testing.T) {
	h := NewHasher(cheapParams())

	encoded, err := h.Hash("short enough")
	require.NoError(t, err)

	huge := strings.Repeat("x", MaxPasswordLength+1)

	_, err = h.Hash(huge)
	assert.ErrorIs(t, err, ErrPasswordTooLong,
		"unbounded input on an unauthenticated endpoint is unbounded work")

	_, err = h.Verify(encoded, huge)
	assert.ErrorIs(t, err, ErrPasswordTooLong)
}

// TestVerify_UnknownEmailCostsSameTime guards the timing oracle that an early
// return on "no such user" would create.
//
// The assertion is deliberately loose. Absolute timings vary with load and
// with the machine, so the test compares medians and allows a wide band: it is
// looking for the difference between "did a derivation" and "returned
// immediately", which is orders of magnitude, not percentage points.
func TestVerify_UnknownEmailCostsSameTime(t *testing.T) {
	// Real parameters, because the cost being equal is the whole assertion.
	h := NewHasher(DefaultParams())

	encoded, err := h.Hash("a known user's password")
	require.NoError(t, err)

	const samples = 5

	known := make([]time.Duration, 0, samples)
	unknown := make([]time.Duration, 0, samples)

	for i := 0; i < samples; i++ {
		// The known-address path: a real row, a wrong password.
		start := time.Now()
		_, err := h.Verify(encoded, "a guess")
		require.NoError(t, err)
		known = append(known, time.Since(start))

		// The unknown-address path: no row at all.
		start = time.Now()
		h.DummyVerify()
		unknown = append(unknown, time.Since(start))
	}

	ratio := float64(median(unknown)) / float64(median(known))
	assert.Greater(t, ratio, 0.5,
		"an unknown address must not answer noticeably faster, that is an account oracle")
	assert.Less(t, ratio, 2.0,
		"an unknown address must not answer noticeably slower either")
}

func TestNeedsRehash_WhenParamsChange(t *testing.T) {
	weak := NewHasher(Params{
		MemoryKiB: 8, Iterations: 1, Threads: 1, SaltLength: 16, KeyLength: 32,
	})
	strong := NewHasher(Params{
		MemoryKiB: 16, Iterations: 2, Threads: 1, SaltLength: 16, KeyLength: 32,
	})

	old, err := weak.Hash("unchanged password")
	require.NoError(t, err)

	// Read by the hasher that produced it, nothing needs doing.
	stale, err := weak.NeedsRehash(old)
	require.NoError(t, err)
	assert.False(t, stale)

	// Read by a hasher configured to cost more, the row is behind.
	stale, err = strong.NeedsRehash(old)
	require.NoError(t, err)
	assert.True(t, stale, "a row hashed under weaker settings must be flagged for upgrade")

	// The old row must still verify, or raising cost would lock every user out.
	ok, err := strong.Verify(old, "unchanged password")
	require.NoError(t, err)
	assert.True(t, ok, "old hashes must keep verifying under their own stored parameters")

	// And a fresh hash from the stronger hasher is not stale.
	fresh, err := strong.Hash("unchanged password")
	require.NoError(t, err)
	stale, err = strong.NeedsRehash(fresh)
	require.NoError(t, err)
	assert.False(t, stale)
}

func TestNewHasher_ZeroParamsFallBackToDefaults(t *testing.T) {
	// A zero Params must never mean "no cost at all", which is what a plain
	// struct literal would otherwise configure.
	h := NewHasher(Params{})
	assert.Equal(t, DefaultParams(), h.Params())
}

func median(ds []time.Duration) time.Duration {
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}
