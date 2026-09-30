package crypto

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGeneratedSecret_Is256BitsOfCSPRNG guards the assumption the whole storage
// decision rests on.
//
// A fast digest is sufficient here only because the secret is full-entropy and
// has no candidate list to guess from. Shorten it, or make it human-readable,
// and the argument for SHA-256 over a deliberately slow derivation collapses
// silently, with nothing else in the system noticing.
func TestGeneratedSecret_Is256BitsOfCSPRNG(t *testing.T) {
	key, err := NewAPIKey()
	require.NoError(t, err)

	_, secret, err := ParseAPIKey(key.Plaintext())
	require.NoError(t, err)

	raw, err := base64.RawURLEncoding.DecodeString(secret)
	require.NoError(t, err, "the secret must be base64url")
	assert.Len(t, raw, 32, "the secret must carry 256 bits")

	// Drawn fresh every time. A generator that repeated itself would make every
	// key issued in that window interchangeable.
	const samples = 500
	seen := make(map[string]struct{}, samples)
	for range samples {
		k, err := NewAPIKey()
		require.NoError(t, err)

		_, s, err := ParseAPIKey(k.Plaintext())
		require.NoError(t, err)

		_, duplicate := seen[s]
		require.False(t, duplicate, "secrets must never repeat")
		seen[s] = struct{}{}

		_, duplicate = seen[k.Prefix()]
		require.False(t, duplicate, "prefixes must never repeat")
		seen[k.Prefix()] = struct{}{}
	}
}

func TestAPIKey_RoundTripsThroughParse(t *testing.T) {
	key, err := NewAPIKey()
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(key.Plaintext(), KeyEnvironmentLive+"_"),
		"a key says which environment it belongs to")

	prefix, secret, err := ParseAPIKey(key.Plaintext())
	require.NoError(t, err)

	assert.Equal(t, key.Prefix(), prefix)
	assert.True(t, SecretMatches(key.Digest(), secret))
}

// TestAPIKey_PrefixCannotContainTheSeparator is why the handle is hex rather
// than base64url like the secret.
//
// The separator between the two halves is an underscore, which is itself a
// character in the base64url alphabet. A base64url handle could contain one and
// make the split ambiguous, which would send a lookup to the wrong row or to no
// row at all, intermittently and only for some keys.
func TestAPIKey_PrefixCannotContainTheSeparator(t *testing.T) {
	for range 200 {
		key, err := NewAPIKey()
		require.NoError(t, err)
		require.NotContains(t, key.Prefix(), "_")
	}
}

func TestParseAPIKey_RejectsMalformed(t *testing.T) {
	good, err := NewAPIKey()
	require.NoError(t, err)

	tests := map[string]string{
		"empty":                 "",
		"no environment":        "abc_def",
		"wrong environment":     "cg_test_" + good.Prefix() + "_secret",
		"environment only":      KeyEnvironmentLive + "_",
		"no separator":          KeyEnvironmentLive + "_" + good.Prefix(),
		"empty secret":          KeyEnvironmentLive + "_" + good.Prefix() + "_",
		"short prefix":          KeyEnvironmentLive + "_abcd_secret",
		"non hex prefix":        KeyEnvironmentLive + "_zzzzzzzzzzzzzzzz_secret",
		"a human session token": "Zm9vYmFyYmF6",
	}

	for name, key := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := ParseAPIKey(key)
			assert.ErrorIs(t, err, ErrMalformedKey)
		})
	}
}

// TestAPIKey_DoesNotPrintItsSecret stops a key reaching a log the first time
// somebody prints a struct that happens to contain one.
func TestAPIKey_DoesNotPrintItsSecret(t *testing.T) {
	key, err := NewAPIKey()
	require.NoError(t, err)

	_, secret, err := ParseAPIKey(key.Plaintext())
	require.NoError(t, err)

	assert.NotContains(t, key.String(), secret)
	assert.NotContains(t, key.LogValue().String(), secret)

	// The handle survives, which is the point: an operator can still say which
	// credential a line refers to.
	assert.Contains(t, key.String(), key.Prefix())
}

// TestSecretMatches_RejectsWrongSecret covers the comparison itself.
//
// There is deliberately no wall-clock assertion here. A timing test on a
// 32-byte comparison is dominated by scheduling noise, so it would be flaky
// while still passing for a non-constant-time implementation. The guarantee
// comes from using subtle.ConstantTimeCompare, which is a property of the code
// a reviewer can check rather than one a test can measure.
func TestSecretMatches_RejectsWrongSecret(t *testing.T) {
	key, err := NewAPIKey()
	require.NoError(t, err)

	_, secret, err := ParseAPIKey(key.Plaintext())
	require.NoError(t, err)

	other, err := NewAPIKey()
	require.NoError(t, err)
	_, otherSecret, err := ParseAPIKey(other.Plaintext())
	require.NoError(t, err)

	assert.True(t, SecretMatches(key.Digest(), secret))
	assert.False(t, SecretMatches(key.Digest(), otherSecret))

	// A secret differing in its first character and one differing in its last
	// are both simply wrong. Neither reveals how much of it matched.
	assert.False(t, SecretMatches(key.Digest(), "x"+secret[1:]))
	assert.False(t, SecretMatches(key.Digest(), secret[:len(secret)-1]+"x"))
	assert.False(t, SecretMatches(key.Digest(), ""))
}
