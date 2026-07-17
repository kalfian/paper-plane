// Package model holds the core domain types shared across the application.
package model

import "time"

// Status represents the lifecycle state of a project.
type Status string

const (
	// StatusActive means the project's site is served publicly.
	StatusActive Status = "active"
	// StatusUnlinked means the project exists but its site returns 404.
	StatusUnlinked Status = "unlinked"
)

// Valid reports whether s is a recognized status value.
func (s Status) Valid() bool {
	switch s {
	case StatusActive, StatusUnlinked:
		return true
	default:
		return false
	}
}

// Project is a single static site managed by Paper Plane.
type Project struct {
	ID        string    // nanoid, also used as the on-disk folder name
	Name      string    // human-friendly display name
	Slug      string    // URL path segment, unique, immutable in MVP
	Status    Status    // active | unlinked
	IndexFile string    // filename served at the site root (default "index.html")
	CreatedAt time.Time // UTC
	UpdatedAt time.Time // UTC
}

// APIKey is a REST API access token. Only KeyHash (hex SHA-256 of the plaintext
// token) is persisted; the plaintext is shown to the operator once at creation.
type APIKey struct {
	ID         string     // nanoid
	Name       string     // human-friendly label
	KeyHash    string     // hex-encoded SHA-256 of the plaintext key
	CreatedAt  time.Time  // UTC
	LastUsedAt *time.Time // UTC; nil until the key is first used
}

// DefaultIndexFile is the landing-page filename used when a project has none set.
const DefaultIndexFile = "index.html"

// EffectiveIndexFile returns IndexFile, falling back to DefaultIndexFile when it
// is empty (e.g. a zero-value Project).
func (p Project) EffectiveIndexFile() string {
	if p.IndexFile == "" {
		return DefaultIndexFile
	}
	return p.IndexFile
}
