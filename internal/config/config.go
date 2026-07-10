// Package config loads runtime configuration from environment variables.
package config

import (
	"os"
	"strings"
)

// Config holds the runtime configuration for Paper Plane, populated from the
// environment. See Load for the environment variable mapping and defaults.
//
// There is no admin-password variable: the admin password is set interactively
// on first run (see the /_app/setup flow) and stored as a bcrypt hash, so no
// secret is ever passed through the environment.
type Config struct {
	// AppURL is the public base URL of the instance (env APP_URL). Optional;
	// used to build absolute links. Trailing slash is trimmed.
	AppURL string
	// DataDir is the directory for persistent data: SQLite DB and site files
	// (env DATA_DIR, default "/data"). Created on Load if missing.
	DataDir string
	// Port is the TCP port the HTTP server listens on (env PORT, default "8080").
	Port string
}

// Load reads configuration from the environment, applies defaults, and ensures
// the data directory exists.
//
// Env mapping:
//   - APP_URL   → AppURL   (optional)
//   - DATA_DIR  → DataDir  (default "/data")
//   - PORT      → Port     (default "8080")
func Load() (Config, error) {
	cfg := Config{
		AppURL:  strings.TrimRight(os.Getenv("APP_URL"), "/"),
		DataDir: envOr("DATA_DIR", "/data"),
		Port:    envOr("PORT", "8080"),
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
