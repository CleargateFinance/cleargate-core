// Package config loads and validates configuration from the environment.
// Loading fails fast at boot rather than surfacing a nil field at 3am.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is the whole application's configuration. Each infrastructure
// package gets only the section it needs, translated by internal/app, so no
// infrastructure package has to import this one.
type Config struct {
	Server   Server
	Database Database
	Cache    Cache
	Log      Log
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
		Log: Log{
			Level:  env("LOG_LEVEL", "info"),
			Format: env("LOG_FORMAT", "text"),
		},
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
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

	return errors.Join(problems...)
}

// env returns the environment variable named key, or fallback when it is unset
// or empty.
func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
