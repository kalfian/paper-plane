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

// APIKey is an access token for the REST API and/or the MCP endpoint. Only
// KeyHash (hex SHA-256 of the plaintext token) is persisted; the plaintext is
// shown to the operator once at creation.
type APIKey struct {
	ID         string     // nanoid
	Name       string     // human-friendly label
	KeyHash    string     // hex-encoded SHA-256 of the plaintext key
	Scope      KeyScope   // which surfaces the key may call (default ScopeAll)
	CreatedAt  time.Time  // UTC
	LastUsedAt *time.Time // UTC; nil until the key is first used
}

// KeyScope limits which surfaces an access token may authenticate against.
type KeyScope string

// Access token scopes.
const (
	// ScopeAll grants both the REST API and the MCP endpoint.
	ScopeAll KeyScope = "all"
	// ScopeAPI grants the REST API (/api/v1/*) only.
	ScopeAPI KeyScope = "api"
	// ScopeMCP grants the MCP endpoint (/api/mcp) only.
	ScopeMCP KeyScope = "mcp"
)

// Valid reports whether s is a known scope.
func (s KeyScope) Valid() bool {
	switch s {
	case ScopeAll, ScopeAPI, ScopeMCP:
		return true
	}
	return false
}

// AllowsAPI reports whether the scope grants the REST API.
func (s KeyScope) AllowsAPI() bool { return s == ScopeAll || s == ScopeAPI }

// AllowsMCP reports whether the scope grants the MCP endpoint.
func (s KeyScope) AllowsMCP() bool { return s == ScopeAll || s == ScopeMCP }

// Label is the human-readable name of the scope shown in the admin UI.
func (s KeyScope) Label() string {
	switch s {
	case ScopeAll:
		return "REST API + MCP"
	case ScopeAPI:
		return "REST API only"
	case ScopeMCP:
		return "MCP only"
	}
	return string(s)
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
