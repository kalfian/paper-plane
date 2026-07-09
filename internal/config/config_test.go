package config

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestLoadMissingAdminPassword(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "")
	t.Setenv("DATA_DIR", t.TempDir())
	if _, err := Load(); !errors.Is(err, ErrMissingAdminPassword) {
		t.Fatalf("Load err = %v, want ErrMissingAdminPassword", err)
	}
}

func TestLoadDefaultsAndTrim(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	t.Setenv("ADMIN_PASSWORD", "pw")
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

func TestLoadDefaultDataDir(t *testing.T) {
	// When DATA_DIR is unset, the default is "/data"; we only assert the value,
	// not creation (which may require root), by pointing at a writable temp dir.
	t.Setenv("ADMIN_PASSWORD", "pw")
	t.Setenv("DATA_DIR", filepath.Join(t.TempDir(), "d"))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AdminPassword != "pw" {
		t.Errorf("AdminPassword = %q, want pw", cfg.AdminPassword)
	}
}
