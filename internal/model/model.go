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
	CreatedAt time.Time // UTC
	UpdatedAt time.Time // UTC
}
