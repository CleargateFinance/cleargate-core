// Package config loads and validates configuration from the environment.
// Loading fails fast at boot rather than surfacing a nil field at 3am.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the whole application's configuration. Each infrastructure
// package gets only the section it needs, translated by internal/app, so no
// infrastructure package has to import this one.
type Config struct {
	Server    Server
	RateLimit RateLimit
	Database  Database
	Cache     Cache
	Crypto    Crypto
	Mail      Mail
	Solana    Solana
	CDP       CDP
	Log       Log
}

// CDP holds the Coinbase Developer Platform credentials that reach account
// wallets. They have no defaults. Only the API signs, so they are checked where
// the API is built rather than here: the worker and the migrate tool must not
// need a secret that can move money.
type CDP struct {
	APIKeyID     string
	APIKeySecret string
	WalletSecret string
	// BaseURL overrides CDP's API root. Empty selects production, which serves
	// devnet and mainnet alike.
	BaseURL string
}

// Solana says which chain money moves on and which token is money there. The
// defaults are devnet: pointing at mainnet must be a decision somebody writes
// down, never what happens when a variable goes missing.
type Solana struct {
	// RPCURL is the JSON-RPC endpoint, which must serve Network.
	RPCURL string
	// Network is the CAIP-2 chain id, the form x402 names networks in.
	Network string
	// USDCMint is the only token accepted, on Network.
	USDCMint string
	// USDCDecimals turns the mint's atomic units into USD. It lives here with
	// the mint and is never read from anything a merchant sends (Task 7.6).
	USDCDecimals int
}

// Server holds HTTP listener settings.
type Server struct {
	// Addr is the host:port the API listens on, for example ":8080".
	Addr string
	// ReadTimeout bounds how long reading a request may take, which stops a
	// slow client from holding a connection open indefinitely.
	ReadTimeout time.Duration
	// WriteTimeout bounds how long writing a response may take.
	WriteTimeout time.Duration
	// ShutdownTimeout bounds how long we wait for in-flight requests to
	// finish during a graceful shutdown before giving up on them.
	ShutdownTimeout time.Duration
	// Mode selects the Gin mode, either "debug" or "release".
	Mode string
	// TrustedProxies are the addresses or CIDR ranges of the load balancers in
	// front of the API, the only peers whose X-Forwarded-For is believed.
	// Empty trusts nobody and uses the connection's own address. Trusting
	// everyone would let any client name its own address and step around the
	// per-address limits and the login throttle.
	TrustedProxies []string
}

// RateLimit caps requests per minute on each surface (Task 6.1).
type RateLimit struct {
	// Public is per client address, on sign-up, login and the other routes
	// that need no credential.
	Public int
	// Agent is per agent, on the agent surface.
	Agent int
	// Console is per signed-in person, on the console surface.
	Console int
}

// Database holds Postgres connection settings.
type Database struct {
	// DSN is the full Postgres connection string.
	DSN string
	// MaxConns caps how many connections the pool keeps open at once.
	MaxConns int32
	// MinConns is how many idle connections the pool keeps warm, so the
	// first request after a quiet period does not pay connection setup cost.
	MinConns int32
	// ConnectTimeout bounds the initial connection attempt at boot.
	ConnectTimeout time.Duration
}

// Cache holds Redis connection settings.
type Cache struct {
	Addr     string
	Password string
	DB       int
}

// Crypto holds the master keys used to encrypt the few secrets the server has
// to be able to read back, such as a TOTP shared secret.
type Crypto struct {
	// MasterKeys maps a key id to a 32-byte key. More than one is loaded only
	// during a rotation, so ciphertext written under a retired key can still be
	// opened while it is being rewrapped.
	MasterKeys map[byte][]byte

	// ActiveKeyID names the key new ciphertexts are sealed under.
	ActiveKeyID byte
}

// Mail selects how the API delivers transactional email. Only the API sends
// any, so it is checked where the API is built rather than here: the worker
// and the migrate tool must not need it.
type Mail struct {
	// Transport has no default. "log" writes the links to the log, which
	// hands out working credentials, so it is accepted only in debug mode.
	// No production transport exists yet, and the API refuses to start in
	// release mode until one does.
	Transport string

	// ConsoleURL is where emailed links point.
	ConsoleURL string
}

// Log holds logging settings.
type Log struct {
	// Level is one of debug, info, warn or error.
	Level string
	// Format is either "json" for production or "text" for local development.
	Format string
}

// Load reads configuration from the environment, applies defaults, then
// validates the result.
//
// Every failure is collected and returned together rather than one at a time,
// so a misconfigured deploy shows all its problems in a single log line
// instead of one per restart.
func Load() (*Config, error) {
	cfg := &Config{
		Server: Server{
			Addr:            env("SERVER_ADDR", ":8080"),
			ReadTimeout:     envDuration("SERVER_READ_TIMEOUT", 10*time.Second),
			WriteTimeout:    envDuration("SERVER_WRITE_TIMEOUT", 10*time.Second),
			ShutdownTimeout: envDuration("SERVER_SHUTDOWN_TIMEOUT", 15*time.Second),
			Mode:            env("SERVER_MODE", "debug"),
			TrustedProxies:  envList("SERVER_TRUSTED_PROXIES"),
		},
		RateLimit: RateLimit{
			Public:  envInt("RATE_LIMIT_PUBLIC_PER_MINUTE", 30),
			Agent:   envInt("RATE_LIMIT_AGENT_PER_MINUTE", 600),
			Console: envInt("RATE_LIMIT_CONSOLE_PER_MINUTE", 300),
		},
		Database: Database{
			DSN:            env("DATABASE_DSN", ""),
			MaxConns:       envInt32("DATABASE_MAX_CONNS", 10),
			MinConns:       envInt32("DATABASE_MIN_CONNS", 2),
			ConnectTimeout: envDuration("DATABASE_CONNECT_TIMEOUT", 10*time.Second),
		},
		Cache: Cache{
			Addr:     env("CACHE_ADDR", "localhost:6379"),
			Password: env("CACHE_PASSWORD", ""),
			DB:       envInt("CACHE_DB", 0),
		},
		Mail: Mail{
			Transport:  os.Getenv("MAIL_TRANSPORT"),
			ConsoleURL: env("CONSOLE_URL", "http://localhost:3000"),
		},
		Solana: Solana{
			RPCURL:       env("SOLANA_RPC_URL", "https://api.devnet.solana.com"),
			Network:      env("SOLANA_NETWORK", "solana:EtWTRABZaYq6iMfeYKouRu166VU2xqa1"),
			USDCMint:     env("SOLANA_USDC_MINT", "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"),
			USDCDecimals: envInt("SOLANA_USDC_DECIMALS", 6),
		},
		CDP: CDP{
			APIKeyID:     os.Getenv("CDP_API_KEY_ID"),
			APIKeySecret: os.Getenv("CDP_API_KEY_SECRET"),
			WalletSecret: os.Getenv("CDP_WALLET_SECRET"),
			BaseURL:      os.Getenv("CDP_BASE_URL"),
		},
		Log: Log{
			Level:  env("LOG_LEVEL", "info"),
			Format: env("LOG_FORMAT", "text"),
		},
	}

	keys, activeID, err := loadMasterKeys()
	if err != nil {
		return nil, err
	}
	cfg.Crypto = Crypto{MasterKeys: keys, ActiveKeyID: activeID}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// loadMasterKeys parses CRYPTO_MASTER_KEYS, a comma-separated list of
// "id:base64key" pairs, and CRYPTO_ACTIVE_KEY_ID.
//
// The id travels in every ciphertext, which is what allows a key to be rotated
// without a migration: new values are sealed under the active key while old
// ones keep naming the key that can still open them.
//
// A parse failure is returned rather than defaulted. Everywhere else in this
// file a bad value falls back to something sensible, but there is no sensible
// fallback for a missing encryption key, and inventing one would mean either
// refusing to enrol anybody or, far worse, a hardcoded key that ships to
// production and makes every stored secret recoverable by anyone with the
// source.
func loadMasterKeys() (map[byte][]byte, byte, error) {
	raw := os.Getenv("CRYPTO_MASTER_KEYS")
	if raw == "" {
		return nil, 0, nil
	}

	keys := map[byte][]byte{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}

		idText, keyText, found := strings.Cut(pair, ":")
		if !found {
			return nil, 0, errors.New("CRYPTO_MASTER_KEYS entries must be id:base64key")
		}

		id, err := strconv.ParseUint(strings.TrimSpace(idText), 10, 8)
		if err != nil {
			return nil, 0, fmt.Errorf("CRYPTO_MASTER_KEYS has a bad key id %q", idText)
		}

		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keyText))
		if err != nil {
			// The key itself is never echoed, here or anywhere else.
			return nil, 0, fmt.Errorf("CRYPTO_MASTER_KEYS key %d is not valid base64", id)
		}

		keys[byte(id)] = key
	}

	activeID, err := strconv.ParseUint(env("CRYPTO_ACTIVE_KEY_ID", "1"), 10, 8)
	if err != nil {
		return nil, 0, errors.New("CRYPTO_ACTIVE_KEY_ID must be a number from 0 to 255")
	}

	return keys, byte(activeID), nil
}

// validate checks the values that have no safe default and would otherwise
// fail much later, at the first request rather than at boot.
func (c *Config) validate() error {
	var problems []error

	if c.Database.DSN == "" {
		problems = append(problems, errors.New("DATABASE_DSN is required"))
	}
	if c.Database.MinConns > c.Database.MaxConns {
		problems = append(problems, fmt.Errorf(
			"DATABASE_MIN_CONNS (%d) must not exceed DATABASE_MAX_CONNS (%d)",
			c.Database.MinConns, c.Database.MaxConns))
	}
	if c.Server.Mode != "debug" && c.Server.Mode != "release" {
		problems = append(problems, fmt.Errorf(
			"SERVER_MODE must be debug or release, got %q", c.Server.Mode))
	}
	// Bounded by what the payment table can hold exactly, numeric(38, 18).
	if c.Solana.USDCDecimals < 1 || c.Solana.USDCDecimals > 18 {
		problems = append(problems, fmt.Errorf(
			"SOLANA_USDC_DECIMALS must be between 1 and 18, got %d", c.Solana.USDCDecimals))
	}
	for _, p := range c.Server.TrustedProxies {
		if _, err := netip.ParsePrefix(p); err != nil {
			if _, err := netip.ParseAddr(p); err != nil {
				problems = append(problems, fmt.Errorf(
					"SERVER_TRUSTED_PROXIES entry %q is not an address or CIDR range", p))
			}
		}
	}
	for name, limit := range map[string]int{
		"RATE_LIMIT_PUBLIC_PER_MINUTE":  c.RateLimit.Public,
		"RATE_LIMIT_AGENT_PER_MINUTE":   c.RateLimit.Agent,
		"RATE_LIMIT_CONSOLE_PER_MINUTE": c.RateLimit.Console,
	} {
		if limit <= 0 {
			problems = append(problems, fmt.Errorf("%s must be a positive number, got %d", name, limit))
		}
	}

	// Every key is checked, not just the active one. A retired key that decodes
	// to the wrong length would otherwise pass here and fail much later, at the
	// one moment it is genuinely needed: opening an old row during a rotation.
	if len(c.Crypto.MasterKeys) == 0 {
		problems = append(problems, errors.New(
			"CRYPTO_MASTER_KEYS is required, generate one with: openssl rand -base64 32"))
	} else {
		if _, ok := c.Crypto.MasterKeys[c.Crypto.ActiveKeyID]; !ok {
			problems = append(problems, fmt.Errorf(
				"CRYPTO_ACTIVE_KEY_ID is %d but CRYPTO_MASTER_KEYS has no key with that id",
				c.Crypto.ActiveKeyID))
		}
		for keyID, key := range c.Crypto.MasterKeys {
			if len(key) != masterKeySize {
				problems = append(problems, fmt.Errorf(
					"CRYPTO_MASTER_KEYS key %d must decode to %d bytes, got %d",
					keyID, masterKeySize, len(key)))
			}
		}
	}

	return errors.Join(problems...)
}

// masterKeySize is the key length AES-256 requires. It is repeated here rather
// than imported from the crypto package, because config is deliberately
// depended upon by nothing and depends on nothing.
const masterKeySize = 32

// env returns the environment variable named key, or fallback when it is unset
// or empty.
func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envList splits a comma-separated variable, dropping empty entries, and is
// nil when the variable is unset.
func envList(key string) []string {
	var out []string
	for _, v := range strings.Split(os.Getenv(key), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// envInt behaves like env but parses an integer, falling back on any parse error.
func envInt(key string, fallback int) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return fallback
	}
	return v
}

// envInt32 behaves like envInt but parses directly into an int32.
//
// ParseInt is given a 32-bit width up front, so a value outside int32's range
// is a parse error and falls back to the default. Parsing as a plain int and
// narrowing afterward would let an out-of-range value silently wrap into a
// different, possibly negative, number instead of being caught.
func envInt32(key string, fallback int32) int32 {
	v, err := strconv.ParseInt(os.Getenv(key), 10, 32)
	if err != nil {
		return fallback
	}
	return int32(v)
}

// envDuration behaves like env but parses a Go duration such as "10s".
func envDuration(key string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(os.Getenv(key))
	if err != nil {
		return fallback
	}
	return d
}
