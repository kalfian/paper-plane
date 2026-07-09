package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/kalfian/paper-plane/internal/model"

	// Register the pure-Go SQLite driver under the name "sqlite".
	_ "modernc.org/sqlite"
)

// timeLayout is the canonical string layout used to persist timestamps. Storing
// an explicit UTC string avoids driver-specific TIMESTAMP scanning quirks.
const timeLayout = time.RFC3339Nano

// SQLite is a Store backed by a SQLite database via modernc.org/sqlite (pure
// Go, no CGO). It opens the database in WAL mode with a busy timeout so brief
// writer contention does not immediately fail.
type SQLite struct {
	db  *sql.DB
	now func() time.Time // injectable clock; defaults to time.Now
}

// compile-time assertion that SQLite satisfies Store.
var _ Store = (*SQLite)(nil)

// NewSQLite opens (creating if needed) the database at path, applies WAL mode
// and a busy timeout, and runs embedded migrations. The caller must Close it.
func NewSQLite(ctx context.Context, path string) (*SQLite, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	dsn := "file:" + path + "?" + q.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &SQLite{db: db, now: time.Now}, nil
}

// Close closes the underlying database.
func (s *SQLite) Close() error { return s.db.Close() }

// CreateProject inserts p, stamping CreatedAt/UpdatedAt (UTC).
func (s *SQLite) CreateProject(ctx context.Context, p *model.Project) error {
	if p.Status == "" {
		p.Status = model.StatusActive
	}
	now := s.now().UTC()
	p.CreatedAt = now
	p.UpdatedAt = now

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (id, name, slug, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.Slug, string(p.Status),
		now.Format(timeLayout), now.Format(timeLayout),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrSlugExists
		}
		return err
	}
	return nil
}

// GetProject returns the project by id.
func (s *SQLite) GetProject(ctx context.Context, id string) (*model.Project, error) {
	return s.scanOne(s.db.QueryRowContext(ctx, selectProject+` WHERE id = ?`, id))
}

// GetProjectBySlug returns the project by slug.
func (s *SQLite) GetProjectBySlug(ctx context.Context, slug string) (*model.Project, error) {
	return s.scanOne(s.db.QueryRowContext(ctx, selectProject+` WHERE slug = ?`, slug))
}

// ListProjects returns all projects, newest first.
func (s *SQLite) ListProjects(ctx context.Context) ([]model.Project, error) {
	rows, err := s.db.QueryContext(ctx, selectProject+` ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []model.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// UpdateProject updates the mutable fields (name) and bumps updated_at.
func (s *SQLite) UpdateProject(ctx context.Context, p *model.Project) error {
	now := s.now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET name = ?, updated_at = ? WHERE id = ?`,
		p.Name, now.Format(timeLayout), p.ID,
	)
	if err != nil {
		return err
	}
	if err := requireOneRow(res); err != nil {
		return err
	}
	p.UpdatedAt = now
	return nil
}

// SetStatus updates the project status and bumps updated_at.
func (s *SQLite) SetStatus(ctx context.Context, id string, status model.Status) error {
	if !status.Valid() {
		return fmt.Errorf("store: invalid status %q", status)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET status = ?, updated_at = ? WHERE id = ?`,
		string(status), s.now().UTC().Format(timeLayout), id,
	)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

// DeleteProject removes the project row.
func (s *SQLite) DeleteProject(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

// GetSetting returns the value for key.
func (s *SQLite) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// SetSetting upserts key=value.
func (s *SQLite) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	return err
}

const selectProject = `SELECT id, name, slug, status, created_at, updated_at FROM projects`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanOne scans a single row, mapping sql.ErrNoRows to ErrNotFound.
func (s *SQLite) scanOne(row *sql.Row) (*model.Project, error) {
	p, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// scanProject reads the projects columns in selectProject order.
func scanProject(sc rowScanner) (*model.Project, error) {
	var (
		p                  model.Project
		status             string
		createdStr, updStr string
	)
	if err := sc.Scan(&p.ID, &p.Name, &p.Slug, &status, &createdStr, &updStr); err != nil {
		return nil, err
	}
	p.Status = model.Status(status)

	var err error
	if p.CreatedAt, err = time.Parse(timeLayout, createdStr); err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	if p.UpdatedAt, err = time.Parse(timeLayout, updStr); err != nil {
		return nil, fmt.Errorf("parse updated_at: %w", err)
	}
	return &p, nil
}

// requireOneRow returns ErrNotFound if the statement affected zero rows.
func requireOneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// isUniqueViolation reports whether err is a SQLite UNIQUE constraint failure.
// modernc.org/sqlite surfaces this in the error message; matching on the text
// keeps us from importing the driver's internal error types.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE")
}
