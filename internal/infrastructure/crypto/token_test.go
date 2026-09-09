package crypto_test

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/crypto"
)

// TestSessionToken_HasFullEntropy is the load-bearing assumption behind
// digesting these with SHA-256 rather than argon2id. Shorten the token, or
// make it human-readable, and that reasoning collapses.
func TestSessionToken_HasFullEntropy(t *testing.T) {
	tok, err := crypto.NewToken()
	require.NoError(t, err)

	raw, err := base64.RawURLEncoding.DecodeString(tok.Plaintext())
	require.NoError(t, err, "the token must be valid base64url, safe in cookies and URLs")
	assert.Len(t, raw, crypto.TokenEntropyBytes, "a session token must carry 256 bits")

	// Distinctness across many draws is a cheap smoke test that the source is
	// actually random rather than, say, a counter or a zeroed buffer.
	const draws = 1000
	seen := make(map[string]struct{}, draws)
	for i := 0; i < draws; i++ {
		next, err := crypto.NewToken()
		require.NoError(t, err)
		_, dup := seen[next.Plaintext()]
		require.False(t, dup, "tokens must never repeat")
		seen[next.Plaintext()] = struct{}{}
	}
}

func TestToken_DigestMatchesPlaintext(t *testing.T) {
	tok, err := crypto.NewToken()
	require.NoError(t, err)

	want := sha256.Sum256([]byte(tok.Plaintext()))
	assert.Equal(t, want[:], tok.Digest(),
		"the digest must be derivable from the plaintext, or lookup cannot work")

	assert.Equal(t, tok.Digest(), crypto.DigestToken(tok.Plaintext()),
		"digesting a presented token must reproduce the stored value")
}

func TestToken_DigestIsNotThePlaintext(t *testing.T) {
	tok, err := crypto.NewToken()
	require.NoError(t, err)

	assert.NotEqual(t, tok.Plaintext(), string(tok.Digest()),
		"storing the plaintext would make a database leak a set of live credentials")
}

// TestToken_RedactsItselfInOutput covers the way credentials most often
// escape: a struct interpolated into a log line by someone who was not
// thinking about it.
func TestToken_RedactsItselfInOutput(t *testing.T) {
	tok, err := crypto.NewToken()
	require.NoError(t, err)

	formatted := fmt.Sprintf("%v %s %+v", tok, tok, tok)
	assert.NotContains(t, formatted, tok.Plaintext(),
		"fmt verbs must not print the token")

	logged := slog.AnyValue(tok).String()
	assert.NotContains(t, logged, tok.Plaintext(),
		"structured logging must not print the token")
}
