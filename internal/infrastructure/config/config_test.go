package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// required sets the variables that have no default, so a test changes only
// what it is about.
func required(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/test")
	t.Setenv("CRYPTO_MASTER_KEYS", "1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
}

func TestLoad_TrustsNoProxyByDefault(t *testing.T) {
	required(t)

	cfg, err := Load()

	require.NoError(t, err)
	assert.Empty(t, cfg.Server.TrustedProxies, "no forwarded address is believed unless a proxy is named")
	assert.Positive(t, cfg.RateLimit.Public)
	assert.Positive(t, cfg.RateLimit.Agent)
	assert.Positive(t, cfg.RateLimit.Console)
}

func TestLoad_ReadsTrustedProxies(t *testing.T) {
	required(t)
	t.Setenv("SERVER_TRUSTED_PROXIES", " 10.0.0.0/8, ,192.0.2.7 ")

	cfg, err := Load()

	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.0/8", "192.0.2.7"}, cfg.Server.TrustedProxies)
}

func TestLoad_RefusesBadProxiesAndLimits(t *testing.T) {
	for name, env := range map[string][2]string{
		"proxy that is not an address": {"SERVER_TRUSTED_PROXIES", "load-balancer.internal"},
		"zero public limit":            {"RATE_LIMIT_PUBLIC_PER_MINUTE", "0"},
		"negative agent limit":         {"RATE_LIMIT_AGENT_PER_MINUTE", "-5"},
	} {
		t.Run(name, func(t *testing.T) {
			required(t)
			t.Setenv(env[0], env[1])

			_, err := Load()

			require.Error(t, err)
			assert.Contains(t, err.Error(), env[0])
		})
	}
}
