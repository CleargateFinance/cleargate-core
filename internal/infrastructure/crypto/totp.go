package crypto

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
	"github.com/pquerna/otp/totp"
)

// TOTP settings, following RFC 6238 and what every authenticator app expects.
//
// The period and digit count are effectively fixed by the ecosystem rather than
// chosen: an app shown a 60-second period through a provisioning URI will honour
// it, but a user typing the secret in by hand will not, and the resulting codes
// silently never match.
const (
	// TOTPPeriod is the length of one step, in seconds.
	TOTPPeriod uint = 30

	// TOTPDigits is the length of a generated code.
	TOTPDigits = otp.DigitsSix

	// TOTPSkew is how many steps either side of the current one are accepted.
	//
	// One step, so a code remains usable for roughly ninety seconds in total.
	// Zero would reject anyone whose phone clock drifts by a few seconds or who
	// types slowly, which generates support load without buying much: the
	// replay guard, not the window width, is what stops a captured code from
	// being reused.
	TOTPSkew uint = 1

	// TOTPSecretSize is how many random bytes back a secret, before base32
	// encoding. Twenty is the RFC 4226 recommendation and what HMAC-SHA1
	// consumes as a key without truncating or padding.
	TOTPSecretSize uint = 20
)

// ErrInvalidTOTPSecret reports a stored secret that is not valid base32, which
// means a corrupt row rather than a wrong code.
var ErrInvalidTOTPSecret = errors.New("crypto: malformed totp secret")

// TOTPEnrolment is a newly generated factor, before it has been confirmed.
type TOTPEnrolment struct {
	// Secret is the base32 shared secret, to be stored encrypted.
	Secret string

	// URI is the otpauth:// provisioning URI a QR code is rendered from. It
	// contains the secret, so it is shown once and never logged or stored.
	URI string
}

// NewTOTPEnrolment generates a shared secret and the provisioning URI for it.
//
// issuer and accountName are what appear in the authenticator app's list, so
// they should be the product name and the person's address rather than opaque
// identifiers, which is the difference between "Cleargate: ann@example.com" and
// an unlabelled six-digit code the user cannot match to anything.
func NewTOTPEnrolment(issuer, accountName string) (TOTPEnrolment, error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: accountName,
		Period:      TOTPPeriod,
		SecretSize:  TOTPSecretSize,
		Digits:      TOTPDigits,
		Algorithm:   otp.AlgorithmSHA1,
	})
	if err != nil {
		return TOTPEnrolment{}, fmt.Errorf("crypto: generate totp secret: %w", err)
	}

	return TOTPEnrolment{Secret: key.Secret(), URI: key.URL()}, nil
}

// TOTPStep reports which counter step a moment falls in.
//
// The step is the value the replay guard records, so it has to be derived the
// same way here and in verification: seconds since the epoch divided by the
// period.
func TOTPStep(at time.Time) int64 {
	return at.Unix() / int64(TOTPPeriod)
}

// VerifyTOTP checks a code against a secret and reports which step matched.
//
// The returned step is the whole reason this exists rather than a direct call
// to the library's validator. A code is valid for its entire window, so an
// attacker who observes one — over a shoulder, through a phishing proxy, in a
// log line — can present it again until that window closes. Answering only
// "valid" leaves the caller no way to tell a first use from a replay. Returning
// the step lets the caller refuse anything at or below the highest step it has
// already accepted, which makes each code usable exactly once.
//
// Every candidate step is checked even after one matches. Stopping early would
// make a code from the previous window verify measurably faster than one from
// the next, and that difference is observable to whoever is submitting codes.
func VerifyTOTP(secret, code string, at time.Time) (int64, bool, error) {
	current := TOTPStep(at)

	var matchedStep int64
	var matched bool

	skew := int64(TOTPSkew)
	for offset := -skew; offset <= skew; offset++ {
		step := current + offset
		if step < 0 {
			// Only reachable with a clock set to the first seconds of 1970, but
			// the conversion below is unsigned and would turn a negative step
			// into an enormous counter rather than failing.
			continue
		}

		// A step is a counter, and HOTP over that counter is exactly what TOTP
		// computes, so this derives the same code the library's own validator
		// would, for a step this function knows the number of.
		want, err := hotp.GenerateCodeCustom(secret, uint64(step), hotp.ValidateOpts{
			Digits:    TOTPDigits,
			Algorithm: otp.AlgorithmSHA1,
		})
		if err != nil {
			// The only failure here is a secret that is not valid base32, which
			// is a corrupt stored value rather than anything the caller did.
			return 0, false, ErrInvalidTOTPSecret
		}

		// Constant time, so the comparison does not leak how many leading
		// digits of a guess were correct.
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			matchedStep = step
			matched = true
		}
	}

	if !matched {
		return 0, false, nil
	}
	return matchedStep, true, nil
}
