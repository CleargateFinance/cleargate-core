package crypto

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id is deliberately slow, because a password is low-entropy and
// guessable offline. These are the OWASP defaults for the second recommended
// configuration: 19 MiB of memory, two passes, one lane.
//
// Memory is the parameter that matters most against GPU and ASIC attackers,
// since memory is the resource they cannot cheaply parallelise.
const (
	defaultMemoryKiB  uint32 = 19 * 1024 // 19456 KiB
	defaultIterations uint32 = 2
	defaultThreads    uint8  = 1
	defaultSaltLength uint32 = 16
	defaultKeyLength  uint32 = 32
)

// MaxPasswordLength bounds what will be hashed at all.
//
// Argon2 reads the whole input, so without a limit a caller could submit
// megabytes and make the server do unbounded work on an unauthenticated
// endpoint. The bound is generous enough that no real passphrase reaches it.
const MaxPasswordLength = 1024

// ErrInvalidHash reports an encoded hash that is not a well-formed argon2id
// PHC string, for example a truncated column or a value written by some other
// scheme.
var ErrInvalidHash = errors.New("crypto: malformed password hash")

// ErrPasswordTooLong reports a password beyond MaxPasswordLength.
var ErrPasswordTooLong = errors.New("crypto: password exceeds maximum length")

// Params are the cost settings a hash was, or will be, produced with.
type Params struct {
	// MemoryKiB is how much memory a single derivation allocates.
	MemoryKiB uint32
	// Iterations is how many passes are made over that memory.
	Iterations uint32
	// Threads is how many lanes run in parallel.
	Threads uint8
	// SaltLength is how many random bytes are drawn per password.
	SaltLength uint32
	// KeyLength is the size of the derived hash.
	KeyLength uint32
}

// DefaultParams returns the cost settings new hashes are produced with.
func DefaultParams() Params {
	return Params{
		MemoryKiB:  defaultMemoryKiB,
		Iterations: defaultIterations,
		Threads:    defaultThreads,
		SaltLength: defaultSaltLength,
		KeyLength:  defaultKeyLength,
	}
}

// Hasher hashes and verifies passwords with one set of cost parameters.
//
// The parameters are a field rather than a constant so that tests can run
// cheaply, and so raising the cost later is a configuration change rather than
// a code change.
type Hasher struct {
	params Params
}

// NewHasher builds a Hasher. A zero Params selects the defaults, so callers
// that do not care about cost cannot accidentally configure it to nothing.
func NewHasher(p Params) *Hasher {
	def := DefaultParams()
	if p.MemoryKiB == 0 {
		p.MemoryKiB = def.MemoryKiB
	}
	if p.Iterations == 0 {
		p.Iterations = def.Iterations
	}
	if p.Threads == 0 {
		p.Threads = def.Threads
	}
	if p.SaltLength == 0 {
		p.SaltLength = def.SaltLength
	}
	if p.KeyLength == 0 {
		p.KeyLength = def.KeyLength
	}
	return &Hasher{params: p}
}

// Params reports the cost settings this Hasher produces new hashes with.
func (h *Hasher) Params() Params { return h.params }

// Hash derives a password hash and encodes it as a PHC string.
//
// The encoding carries its own parameters:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>
//
// which is what makes raising the cost later possible at all. A bare hash
// would leave no way to verify rows produced under the old settings, so the
// change would be a migration that cannot read its own data. With the
// parameters inline, old rows keep verifying under their own settings and are
// upgraded opportunistically, on the next successful login.
func (h *Hasher) Hash(password string) (string, error) {
	if len(password) > MaxPasswordLength {
		return "", ErrPasswordTooLong
	}

	salt := make([]byte, h.params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("crypto: read salt: %w", err)
	}

	key := argon2.IDKey(
		[]byte(password), salt,
		h.params.Iterations, h.params.MemoryKiB, h.params.Threads, h.params.KeyLength,
	)

	return encodePHC(h.params, salt, key), nil
}

// Verify reports whether password produced encoded.
//
// The stored parameters are used rather than this Hasher's own, so a hash
// written under older settings still verifies. Comparison is constant time, so
// the duration of a failed attempt does not reveal how much of the hash
// matched.
func (h *Hasher) Verify(encoded, password string) (bool, error) {
	if len(password) > MaxPasswordLength {
		return false, ErrPasswordTooLong
	}

	params, salt, want, err := decodePHC(encoded)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey(
		[]byte(password), salt,
		params.Iterations, params.MemoryKiB, params.Threads, params.KeyLength,
	)

	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsRehash reports whether encoded was produced with weaker settings than
// this Hasher now uses.
//
// Call it after a successful verification: at that moment the plaintext is in
// hand, so the row can be upgraded transparently. That is the entire migration
// strategy for raising cost, and it needs no downtime and no batch job.
func (h *Hasher) NeedsRehash(encoded string) (bool, error) {
	params, _, _, err := decodePHC(encoded)
	if err != nil {
		return false, err
	}

	return params.MemoryKiB < h.params.MemoryKiB ||
		params.Iterations < h.params.Iterations ||
		params.KeyLength < h.params.KeyLength, nil
}

// dummyVerifySink exists so the compiler cannot discard the work DummyVerify
// does. Assigning to a package-level variable is an observable side effect,
// which a local discarded value would not be.
var dummyVerifySink int

// dummyPassword is hashed by DummyVerify. Its value is irrelevant, only that
// deriving from it costs what deriving from a real password costs.
const dummyPassword = "cleargate-timing-equalisation"

// DummyVerify performs the same derivation a real verification would, and
// discards the result.
//
// Login for an unknown address must cost what login for a known one costs.
// Without this, returning early on "no such user" makes the fast responses a
// reliable oracle for which addresses are registered, which is exactly the
// targeting list the identical response bodies exist to withhold.
//
// The equalisation is approximate at the edges: a row still stored under older,
// cheaper parameters verifies faster than this. That gap closes as those rows
// are rehashed on login, and it never widens, because new hashes always use
// current settings.
func (h *Hasher) DummyVerify() {
	// A zero salt is fine here. Nothing is stored or compared, and the cost of
	// a derivation does not depend on the salt's contents.
	salt := make([]byte, h.params.SaltLength)

	key := argon2.IDKey(
		[]byte(dummyPassword), salt,
		h.params.Iterations, h.params.MemoryKiB, h.params.Threads, h.params.KeyLength,
	)

	dummyVerifySink = subtle.ConstantTimeCompare(key, key)
}

// encodePHC renders parameters, salt and hash in the PHC string format.
//
// Base64 is unpadded, as the format requires: '=' is a field separator in some
// PHC dialects, so padding would be ambiguous.
func encodePHC(p Params, salt, key []byte) string {
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		p.MemoryKiB, p.Iterations, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

// decodePHC parses a PHC string back into the parameters it was produced with.
//
// Every failure returns ErrInvalidHash rather than a parse-specific error. A
// caller cannot act differently on "bad base64" than on "wrong algorithm", and
// a detailed error on this path tends to end up in a response body.
func decodePHC(encoded string) (Params, []byte, []byte, error) {
	// A well-formed string splits into six fields, the first of which is empty
	// because the string begins with the separator.
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" {
		return Params{}, nil, nil, ErrInvalidHash
	}

	if parts[1] != "argon2id" {
		return Params{}, nil, nil, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return Params{}, nil, nil, ErrInvalidHash
	}
	// Refusing an unknown version is deliberate. Argon2 revisions have changed
	// the derivation, so verifying one version's hash with another's algorithm
	// would silently reject every correct password.
	if version != argon2.Version {
		return Params{}, nil, nil, ErrInvalidHash
	}

	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d",
		&p.MemoryKiB, &p.Iterations, &p.Threads); err != nil {
		return Params{}, nil, nil, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, ErrInvalidHash
	}

	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, ErrInvalidHash
	}

	p.SaltLength = uint32(len(salt))
	p.KeyLength = uint32(len(key))

	return p, salt, key, nil
}
