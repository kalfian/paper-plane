// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"os"
	"strings"
)

// Config holds the runtime configuration for Paper Plane, populated from the
// environment. See Load for the environment variable mapping and defaults.
type Config struct {
	// AppURL is the public base URL of the instance (env APP_URL). Optional;
	// used to build absolute links. Trailing slash is trimmed.
	AppURL string
	// AdminPassword is the plaintext admin password (env ADMIN_PASSWORD). It is
	// only read at bootstrap to derive the bcrypt hash stored in settings; it is
	// required and never persisted verbatim.
	AdminPassword string
	// DataDir is the directory for persistent data: SQLite DB and site files
	// (env DATA_DIR, default "/data"). Created on Load if missing.
	DataDir string
	// Port is the TCP port the HTTP server listens on (env PORT, default "8080").
	Port string
}

// ErrMissingAdminPassword is returned by Load when ADMIN_PASSWORD is empty.
var ErrMissingAdminPassword = errors.New("config: ADMIN_PASSWORD is required")

// Load reads configuration from the environment, applies defaults, ensures the
// data directory exists, and validates required fields.
//
// Env mapping:
//   - APP_URL         → AppURL        (optional)
//   - ADMIN_PASSWORD  → AdminPassword (required, non-empty)
//   - DATA_DIR        → DataDir       (default "/data")
//   - PORT            → Port          (default "8080")
func Load() (Config, error) {
	cfg := Config{
		AppURL:        strings.TrimRight(os.Getenv("APP_URL"), "/"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),
		DataDir:       envOr("DATA_DIR", "/data"),
		Port:          envOr("PORT", "8080"),
	}

	if cfg.AdminPassword == "" {
		return Config{}, ErrMissingAdminPassword
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// envOr returns the value of the environment variable key, or def if unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
