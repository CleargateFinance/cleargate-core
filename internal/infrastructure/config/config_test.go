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

// TestLoad_SolanaDefaultsToDevnet: reaching mainnet must be something somebody
// wrote down, never what a missing variable falls back to.
func TestLoad_SolanaDefaultsToDevnet(t *testing.T) {
	required(t)

	cfg, err := Load()

	require.NoError(t, err)
	assert.Contains(t, cfg.Solana.RPCURL, "devnet")
	assert.Equal(t, "solana:EtWTRABZaYq6iMfeYKouRu166VU2xqa1", cfg.Solana.Network)
	assert.Equal(t, 6, cfg.Solana.USDCDecimals)
}

func TestLoad_RefusesImpossibleTokenDecimals(t *testing.T) {
	for _, decimals := range []string{"0", "19"} {
		required(t)
		t.Setenv("SOLANA_USDC_DECIMALS", decimals)

		_, err := Load()

		assert.ErrorContains(t, err, "SOLANA_USDC_DECIMALS", decimals)
	}
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

// TestLoadMigrate_NeedsOnlyTheDSN: the migrate task is given the database and
// nothing else (Task 13.4), so it must not demand the master keys or the
// custodian's credentials just to start.
func TestLoadMigrate_NeedsOnlyTheDSN(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/test")
	t.Setenv("CRYPTO_MASTER_KEYS", "")

	cfg, err := LoadMigrate()

	require.NoError(t, err)
	assert.Equal(t, "postgres://localhost/test", cfg.DSN)
}

func TestLoadMigrate_RefusesMissingDSN(t *testing.T) {
	t.Setenv("DATABASE_DSN", "")

	_, err := LoadMigrate()

	assert.ErrorContains(t, err, "DATABASE_DSN")
}

// TestLoadMigrate_DirDefaultsToTheRepository keeps `make migrate-up`, run from
// the repository root, working unchanged; the image sets MIGRATIONS_DIR.
func TestLoadMigrate_DirDefaultsToTheRepository(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/test")
	t.Setenv("MIGRATIONS_DIR", "")

	cfg, err := LoadMigrate()

	require.NoError(t, err)
	assert.Equal(t, "migrations", cfg.Dir)
}

func TestLoadMigrate_ReadsMigrationsDir(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://localhost/test")
	t.Setenv("MIGRATIONS_DIR", "/migrations")

	cfg, err := LoadMigrate()

	require.NoError(t, err)
	assert.Equal(t, "/migrations", cfg.Dir)
}
