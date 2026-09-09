package crypto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
)

// TokenEntropyBytes is how much randomness a bearer token carries.
//
// 256 bits from a CSPRNG puts guessing beyond reach permanently, which is the
// property that lets these be digested with a fast hash rather than argon2id.
const TokenEntropyBytes = 32

// Token is a freshly minted bearer credential: the value handed to the client
// once, and the digest that is stored instead of it.
//
// Both halves come back together so that a caller cannot mint a token and
// forget to store its digest, or store the plaintext by mistake. The plaintext
// is unexported and reachable only through Plaintext, so handing it out is
// always a deliberate act rather than a struct literal away.
type Token struct {
	plaintext string
	digest    []byte
}

// NewToken mints a bearer token and its digest.
//
// The plaintext exists only in this return value and in whatever the caller
// sends to the client. Nothing persists it, so a database disclosure yields
// digests that cannot be replayed as credentials.
func NewToken() (Token, error) {
	raw := make([]byte, TokenEntropyBytes)
	if _, err := rand.Read(raw); err != nil {
		return Token{}, fmt.Errorf("crypto: read token entropy: %w", err)
	}

	// base64url so the value is safe in a cookie, a header and a URL without
	// further escaping.
	plaintext := base64.RawURLEncoding.EncodeToString(raw)

	return Token{plaintext: plaintext, digest: DigestToken(plaintext)}, nil
}

// Plaintext returns the value to send to the client. Never store it.
func (t Token) Plaintext() string { return t.plaintext }

// Digest returns the value to store and to look up by.
func (t Token) Digest() []byte { return t.digest }

// String redacts the token.
//
// Credentials leak through logs far more often than through databases, usually
// because a struct was interpolated into a message by a caller who never
// thought about it. Implementing Stringer means the obvious mistake prints
// nothing useful.
func (t Token) String() string { return "crypto.Token{REDACTED}" }

// LogValue redacts the token for structured logging, covering slog.Any in the
// same way String covers fmt verbs.
func (t Token) LogValue() slog.Value { return slog.StringValue("REDACTED") }

// DigestToken hashes a bearer token for storage and lookup.
//
// SHA-256 rather than argon2id, and the reason is the entropy of the secret,
// not how valuable what it protects is. Argon2id is slow on purpose to make
// guessing a human-chosen password expensive. There is no candidate list to
// guess from here, so slowness would buy nothing and cost a great deal: this
// digest is computed on every authenticated request, where argon2id's tens of
// milliseconds and megabytes would consume the latency budget and hand an
// attacker an amplification vector.
//
// The requirements a digest must actually meet here are that a leaked table
// yields no usable credentials, and that lookup stays indexable. A fast
// cryptographic hash satisfies both.
func DigestToken(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}
