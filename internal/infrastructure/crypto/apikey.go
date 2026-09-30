package crypto

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// API key format: cg_live_<prefix>_<secret>
//
// Two parts doing two jobs. The prefix is a non-secret handle that finds
// exactly one row, because a lookup has to be indexable and a hash of a secret
// is not something you can index by. The secret is what proves the caller holds
// the key, and only its digest is stored.
//
// That split is also what makes a key safe to talk about. A support engineer
// reading a log or a dashboard can identify which credential is misbehaving,
// and revoke it, without ever handling the secret half.
const (
	// KeyEnvironmentLive labels keys that move real money. It is part of the
	// key text so that a key pasted into the wrong environment's configuration
	// fails on inspection rather than by doing something unintended.
	KeyEnvironmentLive = "cg_live"

	// keyPrefixBytes is the entropy behind the handle. Eight bytes makes an
	// accidental collision a non-event at any plausible number of credentials,
	// and the unique index behind it turns one into an error rather than a
	// misdirected lookup.
	keyPrefixBytes = 8

	// keySecretBytes is the secret itself. Two hundred and fifty-six bits from
	// the system generator is the assumption the whole storage decision rests
	// on: there is no candidate list to guess from, which is why a fast digest
	// is sufficient and a deliberately slow one would only cost latency.
	keySecretBytes = 32
)

// ErrMalformedKey reports a key that is not in the expected format. Every
// malformed shape returns this one error, because a caller can do nothing
// different for a bad prefix than for a missing separator.
var ErrMalformedKey = errors.New("crypto: malformed api key")

// APIKey is a freshly minted agent credential.
//
// The secret exists in this value and in whatever the caller does with it, and
// nowhere else. It is never stored, never logged, and cannot be recovered, so
// losing it means rotating rather than looking it up.
type APIKey struct {
	prefix string
	secret string
}

// NewAPIKey mints a credential.
func NewAPIKey() (APIKey, error) {
	prefixRaw := make([]byte, keyPrefixBytes)
	if _, err := rand.Read(prefixRaw); err != nil {
		return APIKey{}, fmt.Errorf("crypto: read key prefix: %w", err)
	}

	secretRaw := make([]byte, keySecretBytes)
	if _, err := rand.Read(secretRaw); err != nil {
		return APIKey{}, fmt.Errorf("crypto: read key secret: %w", err)
	}

	// The prefix is hex rather than base64url, and that is load bearing rather
	// than cosmetic. The separator between the two halves is an underscore,
	// which is itself a character in the base64url alphabet, so a base64url
	// prefix could contain one and make the split ambiguous. Hex cannot.
	return APIKey{
		prefix: hex.EncodeToString(prefixRaw),
		secret: base64.RawURLEncoding.EncodeToString(secretRaw),
	}, nil
}

// Prefix returns the non-secret handle, which is what gets stored, indexed and
// shown to a human identifying a credential.
func (k APIKey) Prefix() string { return k.prefix }

// Plaintext returns the whole key, exactly once, for handing to whoever asked
// for it. Naming it explicitly means every place the secret escapes is greppable.
func (k APIKey) Plaintext() string {
	return KeyEnvironmentLive + "_" + k.prefix + "_" + k.secret
}

// Digest returns what is stored in place of the secret.
func (k APIKey) Digest() []byte { return DigestSecret(k.secret) }

// String deliberately does not return the key.
//
// Without this, a key reaches a log the first time somebody prints a struct
// that happens to contain one, which is how credentials end up in log
// aggregation with a much longer retention than anybody intended.
func (k APIKey) String() string {
	return KeyEnvironmentLive + "_" + k.prefix + "_[redacted]"
}

// LogValue is what slog records, for the same reason.
func (k APIKey) LogValue() slog.Value { return slog.StringValue(k.String()) }

// DigestSecret hashes the secret half of a key.
//
// SHA-256 rather than argon2id, and the reason is the entropy of the input
// rather than the value of what it protects. A password is human-chosen and
// guessable from a candidate list, so a deliberately slow derivation makes that
// search expensive. This secret is 256 bits from the system generator, where no
// such list exists and no work factor makes an already infeasible search
// harder.
//
// A slow hash here would also be actively harmful. An agent presents its key on
// every request and has no session to amortise the cost over, and a well-formed
// prefix with a garbage secret would be enough to make the server burn memory
// rejecting it.
func DigestSecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// ParseAPIKey splits a presented key into its handle and its secret.
//
// It validates shape only. Whether the credential exists, is revoked, or
// belongs to a live agent are all questions for the store, and answering any of
// them here would put authorisation in two places.
func ParseAPIKey(key string) (prefix, secret string, err error) {
	rest, found := strings.CutPrefix(key, KeyEnvironmentLive+"_")
	if !found {
		return "", "", ErrMalformedKey
	}

	prefix, secret, found = strings.Cut(rest, "_")
	if !found || prefix == "" || secret == "" {
		return "", "", ErrMalformedKey
	}

	// The prefix is the lookup key, so a shape that could never have been
	// issued is rejected before it reaches the database rather than becoming a
	// query that matches nothing.
	if len(prefix) != hex.EncodedLen(keyPrefixBytes) {
		return "", "", ErrMalformedKey
	}
	if _, err := hex.DecodeString(prefix); err != nil {
		return "", "", ErrMalformedKey
	}

	return prefix, secret, nil
}

// SecretMatches reports whether a presented secret produced a stored digest.
//
// Constant time, so the duration of a rejection does not reveal how many
// leading bytes of a guess were right. That matters more here than it looks:
// the prefix names one row, so an attacker who wanted to grind a secret would
// be doing it against a single known target.
func SecretMatches(storedDigest []byte, secret string) bool {
	return subtle.ConstantTimeCompare(storedDigest, DigestSecret(secret)) == 1
}
