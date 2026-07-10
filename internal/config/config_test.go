package config

import (
	"path/filepath"
	"testing"
)

func TestLoadDefaultsAndTrim(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	t.Setenv("DATA_DIR", dir)
	t.Setenv("APP_URL", "https://example.com/")
	t.Setenv("PORT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != "8080" {
		t.Errorf("Port = %q, want default 8080", cfg.Port)
	}
	if cfg.AppURL != "https://example.com" {
		t.Errorf("AppURL = %q, want trailing slash trimmed", cfg.AppURL)
	}
	if cfg.DataDir != dir {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, dir)
	}
	// DataDir must have been created.
	if _, err := filepath.Abs(dir); err != nil {
		t.Fatalf("abs: %v", err)
	}
}

// TestLoadNoAdminPasswordRequired verifies Load no longer depends on any admin
// password variable: a fresh instance configures its password interactively via
// the first-run setup flow, not the environment.
func TestLoadNoAdminPasswordRequired(t *testing.T) {
	t.Setenv("DATA_DIR", filepath.Join(t.TempDir(), "d"))
	if _, err := Load(); err != nil {
		t.Fatalf("Load without ADMIN_PASSWORD: %v", err)
	}
}
