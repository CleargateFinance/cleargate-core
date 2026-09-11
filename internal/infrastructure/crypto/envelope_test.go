package crypto

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testKey draws a master key of the required size.
func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, MasterKeySize)
	_, err := rand.Read(key)
	require.NoError(t, err)
	return key
}

func testKeyring(t *testing.T) *Keyring {
	t.Helper()
	ring, err := NewKeyring(map[byte][]byte{1: testKey(t)}, 1)
	require.NoError(t, err)
	return ring
}

func TestEnvelope_RoundTrips(t *testing.T) {
	ring := testKeyring(t)
	secret := []byte("JBSWY3DPEHPK3PXP")

	sealed, err := ring.Seal(secret, "mfa:totp:user-1")
	require.NoError(t, err)

	opened, err := ring.Open(sealed, "mfa:totp:user-1")
	require.NoError(t, err)
	assert.Equal(t, secret, opened)
}

// TestEnvelope_CiphertextDoesNotContainPlaintext is the most basic property
// worth asserting: that the thing written to the database is not the secret.
func TestEnvelope_CiphertextDoesNotContainPlaintext(t *testing.T) {
	ring := testKeyring(t)
	secret := []byte("JBSWY3DPEHPK3PXP")

	sealed, err := ring.Seal(secret, "mfa:totp:user-1")
	require.NoError(t, err)

	assert.False(t, bytes.Contains(sealed, secret),
		"the sealed value must not carry the secret in the clear")
}

// TestEnvelope_SameSecretSealsDifferently checks that a fresh data key and
// nonce are drawn every time.
//
// Without this, two users who enrol with the same secret would produce byte
// identical rows, and anybody reading the table could tell they matched without
// decrypting anything.
func TestEnvelope_SameSecretSealsDifferently(t *testing.T) {
	ring := testKeyring(t)
	secret := []byte("JBSWY3DPEHPK3PXP")

	first, err := ring.Seal(secret, "mfa:totp:user-1")
	require.NoError(t, err)
	second, err := ring.Seal(secret, "mfa:totp:user-1")
	require.NoError(t, err)

	assert.NotEqual(t, first, second)
}

// TestEnvelope_ContextIsBound is the property that makes a ciphertext
// non-transferable.
//
// Copying a row from one user to another must not yield a working secret,
// because an attacker with write access would otherwise install a factor they
// control against somebody else's login.
func TestEnvelope_ContextIsBound(t *testing.T) {
	ring := testKeyring(t)

	sealed, err := ring.Seal([]byte("JBSWY3DPEHPK3PXP"), "mfa:totp:user-1")
	require.NoError(t, err)

	_, err = ring.Open(sealed, "mfa:totp:user-2")
	assert.ErrorIs(t, err, ErrDecrypt, "a ciphertext must not open under another owner")
}

// TestEnvelope_DetectsTampering covers every region of the format, since a
// forgery in any of them must fail rather than produce a different plaintext.
func TestEnvelope_DetectsTampering(t *testing.T) {
	ring := testKeyring(t)

	sealed, err := ring.Seal([]byte("JBSWY3DPEHPK3PXP"), "mfa:totp:user-1")
	require.NoError(t, err)

	tests := map[string]int{
		"version":     0,
		"key id":      1,
		"wrap nonce":  3,
		"wrapped key": 20,
		"data nonce":  envelopeHeaderSize - 4,
		"payload":     envelopeHeaderSize + 1,
	}

	for name, offset := range tests {
		t.Run(name, func(t *testing.T) {
			tampered := make([]byte, len(sealed))
			copy(tampered, sealed)
			tampered[offset] ^= 0xff

			_, err := ring.Open(tampered, "mfa:totp:user-1")
			assert.ErrorIs(t, err, ErrDecrypt)
		})
	}
}

func TestEnvelope_RejectsTruncatedValue(t *testing.T) {
	ring := testKeyring(t)

	sealed, err := ring.Seal([]byte("JBSWY3DPEHPK3PXP"), "mfa:totp:user-1")
	require.NoError(t, err)

	_, err = ring.Open(sealed[:envelopeHeaderSize], "mfa:totp:user-1")
	assert.ErrorIs(t, err, ErrDecrypt)
}

// TestEnvelope_RetiredKeyStillOpens is the reason a key id travels in the
// ciphertext at all.
//
// During a rotation, rows written under the old key must keep opening, or
// activating a new key would lock everybody out of their own second factor.
func TestEnvelope_RetiredKeyStillOpens(t *testing.T) {
	oldKey, newKey := testKey(t), testKey(t)

	before, err := NewKeyring(map[byte][]byte{1: oldKey}, 1)
	require.NoError(t, err)

	sealed, err := before.Seal([]byte("JBSWY3DPEHPK3PXP"), "mfa:totp:user-1")
	require.NoError(t, err)

	// Rotation: a new active key, with the old one still loaded.
	after, err := NewKeyring(map[byte][]byte{1: oldKey, 2: newKey}, 2)
	require.NoError(t, err)

	opened, err := after.Open(sealed, "mfa:totp:user-1")
	require.NoError(t, err)
	assert.Equal(t, []byte("JBSWY3DPEHPK3PXP"), opened)
}

// TestEnvelope_RewrapMovesKeyWithoutTouchingPayload is what the second key
// layer buys.
//
// Retiring a master key must not mean decrypting and rewriting every secret, so
// rewrapping has to change only the wrapped data key and leave the payload
// byte for byte identical.
func TestEnvelope_RewrapMovesKeyWithoutTouchingPayload(t *testing.T) {
	oldKey, newKey := testKey(t), testKey(t)
	secret := []byte("JBSWY3DPEHPK3PXP")

	before, err := NewKeyring(map[byte][]byte{1: oldKey}, 1)
	require.NoError(t, err)
	sealed, err := before.Seal(secret, "mfa:totp:user-1")
	require.NoError(t, err)

	after, err := NewKeyring(map[byte][]byte{1: oldKey, 2: newKey}, 2)
	require.NoError(t, err)

	rewrapped, err := after.Rewrap(sealed)
	require.NoError(t, err)

	assert.Equal(t, byte(2), rewrapped[1], "the envelope now names the new key")
	assert.Equal(t, sealed[envelopeHeaderSize:], rewrapped[envelopeHeaderSize:],
		"the payload must not be re-encrypted")

	// And the point of the exercise: the old key can now be dropped entirely.
	onlyNew, err := NewKeyring(map[byte][]byte{2: newKey}, 2)
	require.NoError(t, err)

	opened, err := onlyNew.Open(rewrapped, "mfa:totp:user-1")
	require.NoError(t, err)
	assert.Equal(t, secret, opened)

	// The original, meanwhile, is now unreadable, which is what retiring a key
	// is supposed to mean.
	_, err = onlyNew.Open(sealed, "mfa:totp:user-1")
	assert.ErrorIs(t, err, ErrDecrypt)
}

func TestEnvelope_UnknownKeyIsNotDecryptable(t *testing.T) {
	sealed, err := testKeyring(t).Seal([]byte("secret"), "ctx")
	require.NoError(t, err)

	other, err := NewKeyring(map[byte][]byte{9: testKey(t)}, 9)
	require.NoError(t, err)

	_, err = other.Open(sealed, "ctx")
	assert.ErrorIs(t, err, ErrDecrypt)
}

func TestNewKeyring_RejectsUnusableConfiguration(t *testing.T) {
	t.Run("no keys", func(t *testing.T) {
		_, err := NewKeyring(nil, 1)
		assert.ErrorIs(t, err, ErrNoActiveKey)
	})

	t.Run("active key missing", func(t *testing.T) {
		_, err := NewKeyring(map[byte][]byte{1: testKey(t)}, 2)
		assert.ErrorIs(t, err, ErrNoActiveKey)
	})

	// Every key is checked, not only the active one. A short retired key would
	// otherwise pass here and fail at the one moment it is genuinely needed.
	t.Run("retired key is the wrong size", func(t *testing.T) {
		_, err := NewKeyring(map[byte][]byte{
			1: make([]byte, 16),
			2: testKey(t),
		}, 2)
		assert.ErrorIs(t, err, ErrKeySize)
	})
}
