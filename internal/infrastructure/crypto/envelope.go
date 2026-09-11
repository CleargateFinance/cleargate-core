package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// Envelope encryption, for the small number of secrets the server must be able
// to read back rather than merely compare against.
//
// A password is hashed, because verifying one only requires deciding whether
// two derivations match. A TOTP shared secret cannot be treated that way:
// checking a code means recomputing HMAC over the secret itself, so the
// plaintext has to be recoverable. Hashing it would make verification
// impossible, and storing it in the clear would make a database disclosure
// equivalent to handing over everybody's second factor.
//
// Two key layers rather than one:
//
//	KEK  a master key held in configuration, never written to the database
//	DEK  a fresh random key per secret, stored next to the data it encrypts,
//	     itself encrypted under the KEK
//
// The reason for the second layer is rotation. With a single key, retiring it
// means decrypting and re-encrypting every row, which is a migration that
// touches all the ciphertext and needs the old key available throughout. With a
// DEK per secret, retiring a KEK only rewraps the 48-byte wrapped keys and
// never touches the payloads. That is also the shape a managed KMS expects,
// where the KEK never leaves the service and only ever sees DEKs, so moving
// this master key into one later is a change of Keyring implementation rather
// than a change of storage format.
const (
	// envelopeVersion prefixes every ciphertext. A format change gets a new
	// number, so old rows stay readable instead of failing as corrupt.
	envelopeVersion byte = 1

	// dekSize is 32 bytes, selecting AES-256 for the data layer.
	dekSize = 32

	// nonceSize is what AES-GCM requires, and is generated fresh per seal.
	nonceSize = 12

	// gcmTagSize is the authentication tag AES-GCM appends.
	gcmTagSize = 16

	// wrappedDEKSize is the DEK plus its own authentication tag.
	wrappedDEKSize = dekSize + gcmTagSize

	// envelopeHeaderSize is everything before the data ciphertext: version,
	// key id, wrap nonce, wrapped DEK, and data nonce.
	envelopeHeaderSize = 1 + 1 + nonceSize + wrappedDEKSize + nonceSize
)

// MasterKeySize is the required length of a master key, in bytes.
const MasterKeySize = 32

// Errors reported by the envelope.
//
// Every failure to recover a plaintext returns ErrDecrypt, whatever went wrong.
// A caller can do nothing different for "unknown key id" than for "tampered
// tag", and distinguishing them in an error that may reach a log is how a
// padding-oracle-shaped problem starts.
var (
	// ErrDecrypt reports that a ciphertext could not be recovered.
	ErrDecrypt = errors.New("crypto: cannot decrypt")

	// ErrNoActiveKey reports a keyring built without a usable active key.
	ErrNoActiveKey = errors.New("crypto: keyring has no active key")

	// ErrKeySize reports a master key that is not MasterKeySize bytes.
	ErrKeySize = fmt.Errorf("crypto: master key must be %d bytes", MasterKeySize)
)

// Keyring holds the master keys in force and knows which one is current.
//
// More than one exists at a time only during a rotation: the new key seals
// everything from the moment it becomes active, and the old ones stay loaded so
// rows written under them can still be opened. Once nothing references a
// retired id, it can be dropped from configuration.
type Keyring struct {
	// aeads maps a key id to the AEAD built from that master key. The ciphers
	// are constructed once at boot rather than per operation, since building
	// one does key expansion that would otherwise repeat on every login.
	aeads map[byte]cipher.AEAD

	// activeID names the key new ciphertexts are sealed under.
	activeID byte
}

// NewKeyring builds a Keyring from master keys, keyed by id.
//
// It fails rather than warns when the active id is missing or a key is the
// wrong length, because a keyring that cannot seal is a service that cannot
// enrol anybody, and that should stop a deploy rather than surface at the first
// enrolment attempt.
func NewKeyring(keys map[byte][]byte, activeID byte) (*Keyring, error) {
	if len(keys) == 0 {
		return nil, ErrNoActiveKey
	}
	if _, ok := keys[activeID]; !ok {
		return nil, ErrNoActiveKey
	}

	aeads := make(map[byte]cipher.AEAD, len(keys))
	for keyID, key := range keys {
		if len(key) != MasterKeySize {
			return nil, fmt.Errorf("%w: key %d is %d", ErrKeySize, keyID, len(key))
		}

		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("crypto: master key %d: %w", keyID, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("crypto: master key %d: %w", keyID, err)
		}
		aeads[keyID] = aead
	}

	return &Keyring{aeads: aeads, activeID: activeID}, nil
}

// ActiveKeyID reports which master key new ciphertexts are sealed under.
func (k *Keyring) ActiveKeyID() byte { return k.activeID }

// Seal encrypts plaintext under a fresh data key, and returns the whole
// envelope as one opaque value to store.
//
// context is bound into the data layer as additional authenticated data. It is
// not secret and not stored, it is re-supplied at Open and must match. Passing
// something that identifies the owner, such as "totp:<user id>", is what makes
// a ciphertext copied into another user's row fail to open rather than quietly
// decrypt and hand that user someone else's second factor.
//
// The layout is fixed-width up to the payload:
//
//	 0      version
//	 1      key id
//	 2..13  wrap nonce
//	14..61  wrapped data key
//	62..73  data nonce
//	74..    data ciphertext and tag
func (k *Keyring) Seal(plaintext []byte, context string) ([]byte, error) {
	kek, ok := k.aeads[k.activeID]
	if !ok {
		return nil, ErrNoActiveKey
	}

	dek := make([]byte, dekSize)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("crypto: generate data key: %w", err)
	}

	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("crypto: data key: %w", err)
	}
	dataAEAD, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: data key: %w", err)
	}

	out := make([]byte, envelopeHeaderSize, envelopeHeaderSize+len(plaintext)+gcmTagSize)
	out[0] = envelopeVersion
	out[1] = k.activeID

	wrapNonce := out[2 : 2+nonceSize]
	if _, err := rand.Read(wrapNonce); err != nil {
		return nil, fmt.Errorf("crypto: wrap nonce: %w", err)
	}

	dataNonce := out[envelopeHeaderSize-nonceSize : envelopeHeaderSize]
	if _, err := rand.Read(dataNonce); err != nil {
		return nil, fmt.Errorf("crypto: data nonce: %w", err)
	}

	// The wrap is authenticated over version and key id only, deliberately not
	// over the caller's context. A rotation job rewraps data keys under a new
	// KEK, and it can only do that without decrypting payloads if the wrap does
	// not depend on what each payload happens to be about.
	wrapped := kek.Seal(nil, wrapNonce, dek, out[:2])
	copy(out[2+nonceSize:], wrapped)

	return dataAEAD.Seal(out, dataNonce, plaintext, []byte(context)), nil
}

// Open recovers a plaintext sealed by Seal.
//
// context must be the same value Seal was given. A mismatch fails
// authentication, which is the point: it makes ciphertext non-transferable
// between the rows it was written for.
func (k *Keyring) Open(envelope []byte, context string) ([]byte, error) {
	if len(envelope) < envelopeHeaderSize+gcmTagSize {
		return nil, ErrDecrypt
	}
	if envelope[0] != envelopeVersion {
		return nil, ErrDecrypt
	}

	kek, ok := k.aeads[envelope[1]]
	if !ok {
		// The key that sealed this is not loaded, which during a rotation means
		// it was retired too early. Worth an operator's attention, but not worth
		// telling a caller which of the failure modes this was.
		return nil, ErrDecrypt
	}

	wrapNonce := envelope[2 : 2+nonceSize]
	wrapped := envelope[2+nonceSize : 2+nonceSize+wrappedDEKSize]

	dek, err := kek.Open(nil, wrapNonce, wrapped, envelope[:2])
	if err != nil {
		return nil, ErrDecrypt
	}

	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, ErrDecrypt
	}
	dataAEAD, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrDecrypt
	}

	dataNonce := envelope[envelopeHeaderSize-nonceSize : envelopeHeaderSize]

	plaintext, err := dataAEAD.Open(nil, dataNonce, envelope[envelopeHeaderSize:], []byte(context))
	if err != nil {
		return nil, ErrDecrypt
	}

	return plaintext, nil
}

// Rewrap re-encrypts an envelope's data key under the current active master
// key, leaving the payload untouched.
//
// This is what makes retiring a master key cheap: the data ciphertext, which is
// the large and sensitive part, is never decrypted, so a rotation never holds
// anybody's secret in memory and never needs the per-row context.
func (k *Keyring) Rewrap(envelope []byte) ([]byte, error) {
	if len(envelope) < envelopeHeaderSize+gcmTagSize || envelope[0] != envelopeVersion {
		return nil, ErrDecrypt
	}
	if envelope[1] == k.activeID {
		return envelope, nil
	}

	oldKEK, ok := k.aeads[envelope[1]]
	if !ok {
		return nil, ErrDecrypt
	}
	newKEK, ok := k.aeads[k.activeID]
	if !ok {
		return nil, ErrNoActiveKey
	}

	dek, err := oldKEK.Open(nil,
		envelope[2:2+nonceSize],
		envelope[2+nonceSize:2+nonceSize+wrappedDEKSize],
		envelope[:2])
	if err != nil {
		return nil, ErrDecrypt
	}

	out := make([]byte, len(envelope))
	copy(out, envelope)
	out[1] = k.activeID

	wrapNonce := out[2 : 2+nonceSize]
	if _, err := rand.Read(wrapNonce); err != nil {
		return nil, fmt.Errorf("crypto: wrap nonce: %w", err)
	}

	copy(out[2+nonceSize:], newKEK.Seal(nil, wrapNonce, dek, out[:2]))

	return out, nil
}
