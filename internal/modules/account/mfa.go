package account

import (
	"crypto/rand"
	"math/big"
	"strings"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// MFAFactorType names a kind of second factor. Only TOTP exists here: WebAuthn
// credentials live in their own table, because they authorise individual
// payloads rather than establishing a window.
type MFAFactorType string

// FactorTOTP is a time-based one-time password from an authenticator app.
const FactorTOTP MFAFactorType = "totp"

// RecoveryCodeCount is how many single-use codes are issued at enrolment.
//
// Ten is enough that losing a phone is an inconvenience rather than a lockout,
// and few enough that a person will plausibly store them somewhere deliberate
// instead of ignoring the list.
const RecoveryCodeCount = 10

// Recovery code shape.
//
// The alphabet omits I, L, O and U: the first three are misread as 1 and 0 when
// transcribed by hand, and the fourth is dropped to keep the set from spelling
// anything unfortunate. Two groups of five give fifty bits of entropy, which is
// far beyond guessing over a throttled endpoint while staying short enough that
// somebody will actually write it down.
const (
	recoveryCodeAlphabet   = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	recoveryCodeGroupLen   = 5
	recoveryCodeGroupCount = 2
)

// MFAFactor is one enrolled second factor.
type MFAFactor struct {
	ID     string
	UserID id.UserID
	Type   MFAFactorType

	// SecretEnc is the envelope-encrypted shared secret. It is never decrypted
	// anywhere but inside a verification, and never leaves this process.
	SecretEnc []byte

	// LastUsedStep is the highest counter step already accepted, and is what
	// makes a code single-use. Nil until the first successful verification.
	LastUsedStep *int64

	// ConfirmedAt is nil while enrolment is pending. An unconfirmed factor
	// satisfies nothing, which is what stops a half-finished enrolment from
	// locking somebody out of their own account.
	ConfirmedAt *time.Time

	CreatedAt time.Time
	RevokedAt *time.Time
}

// Confirmed reports whether this factor has completed enrolment.
func (f MFAFactor) Confirmed() bool { return f.ConfirmedAt != nil }

// RecoveryCode is one stored single-use code. Only the hash is kept, so the
// list cannot be recovered from a database disclosure.
type RecoveryCode struct {
	ID   string
	Hash string
}

// Errors reported by the MFA flows.
var (
	// ErrMFANotEnrolled reports that no confirmed factor exists. It is separate
	// from ErrMFACodeInvalid because the caller genuinely must do something
	// different: enrol, rather than try again.
	ErrMFANotEnrolled = apperr.Forbidden("no second factor is enrolled")

	// ErrMFAAlreadyEnrolled reports an enrolment attempt while a confirmed
	// factor is already in place. Replacing one goes through removal, which is
	// step-up gated, so an attacker holding only a session cannot swap the
	// factor out for their own.
	ErrMFAAlreadyEnrolled = apperr.Conflict("a second factor is already enrolled")

	// ErrMFAEnrolmentNotStarted reports a confirmation with nothing pending.
	ErrMFAEnrolmentNotStarted = apperr.Invalid("no enrolment is in progress")

	// ErrMFACodeInvalid covers a wrong code, an expired one, and one already
	// used, identically. A caller can do nothing different for a replay than
	// for a guess, and saying "that code was already used" would confirm to an
	// attacker that they had captured a real one.
	ErrMFACodeInvalid = apperr.Unauthorized("that code is not valid")
)

// Reasons a step-up check can fail, sent to the client so the console knows
// whether to prompt for a code or start an enrolment.
const (
	// ReasonMFARequired means enrol first, there is no factor at all.
	ReasonMFARequired = "mfa_enrolment_required"

	// ReasonStepUpRequired means a factor exists but has not been presented
	// recently enough for this action.
	ReasonStepUpRequired = "mfa_required"
)

// StepUpError reports that an action needs a second factor the caller has not
// satisfied.
//
// It carries a reason code rather than relying on the message, because the
// console has to branch on it: one case opens an enrolment wizard, the other
// asks for six digits. Message text is for humans and will be translated, so
// branching on it would break the first time somebody rewords a string.
type StepUpError struct {
	Reason string
	cause  error
}

func newStepUpError(reason, message string) *StepUpError {
	return &StepUpError{Reason: reason, cause: apperr.Forbidden(message).WithCode(reason)}
}

func (e *StepUpError) Error() string { return e.cause.Error() }

// Unwrap exposes the wrapped application error, so the usual kind-to-status
// mapping still resolves this to a forbidden response.
func (e *StepUpError) Unwrap() error { return e.cause }

// NewRecoveryCodes generates a fresh set of single-use codes.
//
// The codes are returned in plaintext exactly once, to be shown to the person
// enrolling. Only their hashes are stored, so this return value is the only
// copy that will ever exist outside the user's own records.
func NewRecoveryCodes() ([]string, error) {
	codes := make([]string, 0, RecoveryCodeCount)
	for range RecoveryCodeCount {
		code, err := newRecoveryCode()
		if err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	return codes, nil
}

// newRecoveryCode draws one code from the unambiguous alphabet.
func newRecoveryCode() (string, error) {
	var b strings.Builder
	b.Grow(recoveryCodeGroupCount*recoveryCodeGroupLen + recoveryCodeGroupCount - 1)

	limit := big.NewInt(int64(len(recoveryCodeAlphabet)))

	for group := range recoveryCodeGroupCount {
		if group > 0 {
			b.WriteByte('-')
		}
		for range recoveryCodeGroupLen {
			// crypto/rand's Int is used rather than a modulo of a random byte,
			// which would bias the last few letters of the alphabet upward and
			// quietly shrink the entropy this code is supposed to carry.
			n, err := rand.Int(rand.Reader, limit)
			if err != nil {
				return "", err
			}
			b.WriteByte(recoveryCodeAlphabet[n.Int64()])
		}
	}

	return b.String(), nil
}

// NormalizeRecoveryCode puts a typed code into the form it was generated in.
//
// People transcribe these from paper, so lowercase letters and missing or extra
// separators are expected rather than exceptional, and rejecting them would
// produce a support ticket instead of a login.
func NormalizeRecoveryCode(code string) string {
	var b strings.Builder
	b.Grow(len(code))

	for _, r := range strings.ToUpper(strings.TrimSpace(code)) {
		if r == '-' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}

	trimmed := b.String()
	if len(trimmed) != recoveryCodeGroupCount*recoveryCodeGroupLen {
		// Not a shape this ever generated. Returned as-is so verification fails
		// on comparison rather than here, keeping one rejection path.
		return trimmed
	}

	// Re-insert the separator so the value matches what was hashed.
	groups := make([]string, 0, recoveryCodeGroupCount)
	for i := 0; i < len(trimmed); i += recoveryCodeGroupLen {
		groups = append(groups, trimmed[i:i+recoveryCodeGroupLen])
	}
	return strings.Join(groups, "-")
}
