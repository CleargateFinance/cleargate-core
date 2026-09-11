package crypto

import (
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// codeAtStep produces the code an authenticator app would show for a step, so a
// test can drive verification without waiting for real time to pass.
func codeAtStep(t *testing.T, secret string, step int64) string {
	t.Helper()
	code, err := hotp.GenerateCodeCustom(secret, uint64(step), hotp.ValidateOpts{
		Digits:    TOTPDigits,
		Algorithm: otp.AlgorithmSHA1,
	})
	require.NoError(t, err)
	return code
}

func TestNewTOTPEnrolment_ProducesAScannableURI(t *testing.T) {
	enrolment, err := NewTOTPEnrolment("Cleargate", "ann@example.com")
	require.NoError(t, err)

	assert.NotEmpty(t, enrolment.Secret)
	assert.Contains(t, enrolment.URI, "otpauth://totp/")
	assert.Contains(t, enrolment.URI, "issuer=Cleargate")

	// The period has to be in the URI, because an app that assumes the default
	// would silently produce codes that never match if we ever changed it.
	assert.Contains(t, enrolment.URI, "period=30")
}

func TestVerifyTOTP_AcceptsCurrentCodeAndReportsItsStep(t *testing.T) {
	enrolment, err := NewTOTPEnrolment("Cleargate", "ann@example.com")
	require.NoError(t, err)

	now := time.Unix(1_700_000_000, 0)
	want := TOTPStep(now)

	step, ok, err := VerifyTOTP(enrolment.Secret, codeAtStep(t, enrolment.Secret, want), now)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, want, step, "the matched step is what the replay guard records")
}

// TestVerifyTOTP_SkewWindow pins exactly how far either side of now a code is
// accepted.
//
// One step each way, and no further. Widening it would let a captured code
// stay useful for longer, narrowing it would reject people whose phone clock
// drifts by a few seconds.
func TestVerifyTOTP_SkewWindow(t *testing.T) {
	enrolment, err := NewTOTPEnrolment("Cleargate", "ann@example.com")
	require.NoError(t, err)

	now := time.Unix(1_700_000_000, 0)
	current := TOTPStep(now)

	tests := []struct {
		name   string
		offset int64
		accept bool
	}{
		{"two steps early", -2, false},
		{"one step early", -1, true},
		{"current step", 0, true},
		{"one step late", 1, true},
		{"two steps late", 2, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			step, ok, err := VerifyTOTP(
				enrolment.Secret, codeAtStep(t, enrolment.Secret, current+tc.offset), now)
			require.NoError(t, err)
			assert.Equal(t, tc.accept, ok)
			if tc.accept {
				assert.Equal(t, current+tc.offset, step)
			}
		})
	}
}

func TestVerifyTOTP_RejectsWrongCode(t *testing.T) {
	enrolment, err := NewTOTPEnrolment("Cleargate", "ann@example.com")
	require.NoError(t, err)

	now := time.Unix(1_700_000_000, 0)
	correct := codeAtStep(t, enrolment.Secret, TOTPStep(now))

	// A different code of the same shape, so this cannot pass by length alone.
	wrong := "000000"
	if correct == wrong {
		wrong = "111111"
	}

	_, ok, err := VerifyTOTP(enrolment.Secret, wrong, now)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestVerifyTOTP_RejectsMalformedSecret(t *testing.T) {
	_, _, err := VerifyTOTP("not-valid-base32!", "123456", time.Now())
	assert.ErrorIs(t, err, ErrInvalidTOTPSecret)
}

// TestTOTPStep_MatchesPeriod checks the step derivation directly, because the
// replay guard stores this number and a change to it would silently invalidate
// every stored value.
func TestTOTPStep_MatchesPeriod(t *testing.T) {
	// Aligned to the start of a step, so the assertions below measure the
	// period rather than however far into a step an arbitrary timestamp fell.
	base := time.Unix(56_666_667*int64(TOTPPeriod), 0)
	require.Equal(t, base.Unix()%int64(TOTPPeriod), int64(0))

	assert.Equal(t, TOTPStep(base), TOTPStep(base.Add(29*time.Second)),
		"a step covers the whole period")
	assert.Equal(t, TOTPStep(base)+1, TOTPStep(base.Add(30*time.Second)),
		"and advances exactly once per period")
}
