package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requirement(amount, payTo string) json.RawMessage {
	return json.RawMessage(`{"x402Version":2,"accepts":[{"scheme":"exact","amount":"` + amount + `","payTo":"` + payTo + `"}]}`)
}

func TestDescribe_ReadsThePriceInUSDCExactly(t *testing.T) {
	for atomic, want := range map[string]string{
		"10000":   "0.01",
		"500000":  "0.5",
		"5000000": "5",
		"50000":   "0.05",
		"1":       "0.000001",
	} {
		price, payTo := describe(requirement(atomic, "Merchant"))

		assert.Equal(t, want, price, atomic)
		assert.Equal(t, "Merchant", payTo)
	}
}

func TestDescribe_AnUnreadableRequirementIsNarratedAsUnknown(t *testing.T) {
	price, payTo := describe(json.RawMessage(`{"accepts":[]}`))

	assert.Equal(t, "?", price)
	assert.Equal(t, "?", payTo)
}

func TestDecodeHeader_IsBase64JSON(t *testing.T) {
	got, err := decodeHeader(base64.StdEncoding.EncodeToString([]byte(`{"x402Version":2}`)))
	require.NoError(t, err)
	assert.JSONEq(t, `{"x402Version":2}`, string(got))

	_, err = decodeHeader("")
	assert.Error(t, err, "a missing header")
	_, err = decodeHeader(base64.StdEncoding.EncodeToString([]byte("not json")))
	assert.Error(t, err, "not JSON")
	_, err = decodeHeader("%%%")
	assert.Error(t, err, "not base64")
}

func TestLoadEnv_TheEnvironmentWinsOverTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.env")
	require.NoError(t, os.WriteFile(path, []byte("CLEARGATE_API_KEY=from-file\nMERCHANT_URL=http://merchant\n"), 0o600))
	t.Setenv("CLEARGATE_API_KEY", "from-environment")

	env := loadEnv(path)

	assert.Equal(t, "from-environment", env("CLEARGATE_API_KEY", ""))
	assert.Equal(t, "http://merchant", env("MERCHANT_URL", ""))
	assert.Equal(t, "fallback", env("NOT_SET_ANYWHERE", "fallback"))
}

func TestShort_KeepsAnAddressRecognisable(t *testing.T) {
	assert.Equal(t, "9WzD…AWWM", short("9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"))
	assert.Equal(t, "short", short("short"))
}
