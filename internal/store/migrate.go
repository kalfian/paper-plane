package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"sort"

	"github.com/kalfian/paper-plane/migrations"
)

// migrationsFS is the embedded set of .sql files (see the migrations package;
// go:embed cannot traverse "..", so the embed lives beside the files).
var migrationsFS fs.FS = migrations.FS

// migrate runs all embedded migration files in lexical filename order inside a
// single transaction. Migration files are expected to be idempotent
// (CREATE TABLE IF NOT EXISTS, etc.), so re-running is safe. A
// schema_migrations table records applied filenames to skip already-applied
// ones on subsequent starts.
func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name       TEXT PRIMARY KEY,
		applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}

	for _, name := range names {
		applied, err := migrationApplied(ctx, db, name)
		if err != nil {
			return err
		}
		if applied {
			continue
		}

		body, err := fs.ReadFile(migrationsFS, name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		if err := applyMigration(ctx, db, name, string(body)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}

	return nil
}

// migrationNames returns the sorted list of embedded migration filenames.
func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(migrationsFS, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// migrationApplied reports whether the named migration has already run.
func migrationApplied(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations WHERE name = ?`, name).Scan(&one)
	switch err {
	case nil:
		return true, nil
	case sql.ErrNoRows:
		return false, nil
	default:
		return false, err
	}
}

// applyMigration runs one migration body and records it, atomically.
func applyMigration(ctx context.Context, db *sql.DB, name, body string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, body); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (name) VALUES (?)`, name); err != nil {
		return err
	}
	return tx.Commit()
}
