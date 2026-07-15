// Package store defines the persistence interface for Paper Plane and provides
// a SQLite-backed implementation.
package store

import (
	"context"
	"errors"

	"github.com/kalfian/paper-plane/internal/model"
)

// Well-known settings keys.
const (
	// SettingAdminPasswordHash stores the bcrypt hash of the admin password.
	SettingAdminPasswordHash = "admin_password_hash"
	// SettingCookieSecret stores the hex-encoded HMAC secret for signing
	// session and CSRF tokens.
	SettingCookieSecret = "cookie_secret"
)

// Sentinel errors returned by Store implementations.
var (
	// ErrNotFound is returned when a requested row does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrSlugExists is returned when creating/updating a project would violate
	// the unique slug constraint.
	ErrSlugExists = errors.New("store: slug already exists")
)

// Store is the persistence boundary. It is defined as an interface so handlers
// can be tested against a mock. All methods take a context for cancellation.
type Store interface {
	// CreateProject inserts p. p.ID must be set; CreatedAt/UpdatedAt are set by
	// the implementation. Returns ErrSlugExists on a duplicate slug.
	CreateProject(ctx context.Context, p *model.Project) error
	// GetProject returns the project by id, or ErrNotFound.
	GetProject(ctx context.Context, id string) (*model.Project, error)
	// GetProjectBySlug returns the project by slug, or ErrNotFound.
	GetProjectBySlug(ctx context.Context, slug string) (*model.Project, error)
	// ListProjects returns all projects ordered by created_at descending.
	ListProjects(ctx context.Context) ([]model.Project, error)
	// UpdateProject updates the mutable fields (name) of the project with p.ID.
	// Returns ErrNotFound if it does not exist.
	UpdateProject(ctx context.Context, p *model.Project) error
	// SetStatus sets the project status. Returns ErrNotFound if missing.
	SetStatus(ctx context.Context, id string, status model.Status) error
	// SetIndexFile sets the project's landing-page filename (served at the site
	// root). An empty filename resets it to the default. ErrNotFound if missing.
	SetIndexFile(ctx context.Context, id, filename string) error
	// DeleteProject removes the project row. Returns ErrNotFound if missing.
	DeleteProject(ctx context.Context, id string) error

	// GetSetting returns the value for key, or ErrNotFound.
	GetSetting(ctx context.Context, key string) (string, error)
	// SetSetting upserts key=value.
	SetSetting(ctx context.Context, key, value string) error

	// Close releases underlying resources.
	Close() error
}
