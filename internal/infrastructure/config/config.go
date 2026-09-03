// Package config loads and validates configuration from environment and files.
// Loading fails fast at boot rather than surfacing a nil field at 3am.
package config

// Config holds the application's configuration, loaded from environment and
// files at boot.
type Config struct {
	// TODO(scaffold): Server, Postgres, Redis, Crypto, Rails.
}
