package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// apiLog is what `./run.sh api` tees into .state/api.log: slog's text format,
// which quotes the link because it contains "=".
const apiLog = `time=2026-11-08T19:00:00Z level=INFO msg="api listening" addr=:8080
time=2026-11-08T19:00:01Z level=WARN msg="email verification not sent, logging the link instead" to=owner+1@example.com link="http://localhost:3000/verify-email?token=first-token" mailer=logging
time=2026-11-08T19:00:02Z level=WARN msg="email verification not sent, logging the link instead" to=owner+2@example.com link="http://localhost:3000/verify-email?token=other-owner" mailer=logging
time=2026-11-08T19:00:03Z level=WARN msg="password reset not sent, logging the link instead" to=owner+1@example.com link="http://localhost:3000/reset-password?token=a-reset" mailer=logging
time=2026-11-08T19:00:04Z level=WARN msg="email verification not sent, logging the link instead" to=owner+1@example.com link="http://localhost:3000/verify-email?token=abc%2Bdef_-" mailer=logging
`

func writeLog(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestTokenFromLog_TakesTheLatestVerificationLinkForThatAddress(t *testing.T) {
	got := tokenFromLog(writeLog(t, apiLog), "owner+1@example.com")

	assert.Equal(t, "abc+def_-", got, "the newest link, unescaped, and never the reset link")
}

func TestTokenFromLog_IgnoresOtherAddresses(t *testing.T) {
	assert.Equal(t, "other-owner", tokenFromLog(writeLog(t, apiLog), "owner+2@example.com"))
	assert.Empty(t, tokenFromLog(writeLog(t, apiLog), "nobody@example.com"))
}

func TestTokenFromLog_AnUnquotedLinkToo(t *testing.T) {
	line := "level=WARN to=owner+3@example.com link=http://localhost:3000/verify-email?token=plain mailer=logging\n"

	assert.Equal(t, "plain", tokenFromLog(writeLog(t, line), "owner+3@example.com"))
}

func TestTokenFromLog_NoLogIsNoToken(t *testing.T) {
	assert.Empty(t, tokenFromLog(filepath.Join(t.TempDir(), "missing.log"), "owner+1@example.com"))
}

func TestFundAmount_DefaultsToOneAndStaysWithinTheFaucetsLimit(t *testing.T) {
	n, err := fundAmount(nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	n, err = fundAmount([]string{"3"})
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	for _, bad := range []string{"0", "11", "-1", "1.5", "lots"} {
		_, err := fundAmount([]string{bad})
		assert.Error(t, err, bad)
	}
}

// TestMandateRules_PayOnlyTheMerchantInTheScenariosCategories: the scenario's
// payee_not_permitted decline depends on the partner being left off pay_to,
// and its approvals on both categories being listed.
func TestMandateRules_PayOnlyTheMerchantInTheScenariosCategories(t *testing.T) {
	rules := mandateRules("MerchantWallet111")

	assert.Equal(t, []string{"MerchantWallet111"}, rules["pay_to"])
	assert.Equal(t, []string{"market_data", "research"}, rules["categories"])
	assert.Equal(t, "0.60", rules["max_per_tx"])
	assert.Equal(t, "1.00", rules["max_per_day"])
}
