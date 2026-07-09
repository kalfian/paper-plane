package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kalfian/paper-plane/internal/model"
)

// newTestStore opens a SQLite store backed by a temp-file DB, migrated and
// closed automatically at test end.
func newTestStore(t *testing.T) *SQLite {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := NewSQLite(context.Background(), path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestNewID(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := NewID()
		if len(id) != defaultIDSize {
			t.Fatalf("id %q length = %d, want %d", id, len(id), defaultIDSize)
		}
		if seen[id] {
			t.Fatalf("duplicate id generated: %q", id)
		}
		seen[id] = true
		for _, c := range id {
			if !isURLSafe(byte(c)) {
				t.Fatalf("id %q has non-URL-safe char %q", id, c)
			}
		}
	}
}

func isURLSafe(c byte) bool {
	switch {
	case c >= '0' && c <= '9', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		return true
	case c == '-' || c == '_':
		return true
	default:
		return false
	}
}

func TestProjectCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	p := &model.Project{ID: NewID(), Name: "Docs", Slug: "docs"}
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if p.Status != model.StatusActive {
		t.Fatalf("default status = %q, want active", p.Status)
	}
	if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
		t.Fatal("timestamps not set on create")
	}

	// Get by id.
	got, err := s.GetProject(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Slug != "docs" || got.Name != "Docs" {
		t.Fatalf("GetProject mismatch: %+v", got)
	}

	// Get by slug.
	bySlug, err := s.GetProjectBySlug(ctx, "docs")
	if err != nil {
		t.Fatalf("GetProjectBySlug: %v", err)
	}
	if bySlug.ID != p.ID {
		t.Fatalf("GetProjectBySlug id = %q, want %q", bySlug.ID, p.ID)
	}

	// Update name.
	got.Name = "Documentation"
	if err := s.UpdateProject(ctx, got); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	reread, _ := s.GetProject(ctx, p.ID)
	if reread.Name != "Documentation" {
		t.Fatalf("name after update = %q, want Documentation", reread.Name)
	}

	// List.
	list, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListProjects len = %d, want 1", len(list))
	}

	// Delete.
	if err := s.DeleteProject(ctx, p.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := s.GetProject(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetProject after delete err = %v, want ErrNotFound", err)
	}
}

func TestCreateProjectDuplicateSlug(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.CreateProject(ctx, &model.Project{ID: NewID(), Name: "A", Slug: "same"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	err := s.CreateProject(ctx, &model.Project{ID: NewID(), Name: "B", Slug: "same"})
	if !errors.Is(err, ErrSlugExists) {
		t.Fatalf("duplicate slug err = %v, want ErrSlugExists", err)
	}
}

func TestSetStatus(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	p := &model.Project{ID: NewID(), Name: "Site", Slug: "site"}
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}

	tests := []struct {
		name    string
		id      string
		status  model.Status
		wantErr error
	}{
		{name: "unlink", id: p.ID, status: model.StatusUnlinked},
		{name: "relink", id: p.ID, status: model.StatusActive},
		{name: "invalid status", id: p.ID, status: model.Status("bogus"), wantErr: errInvalidStatusSentinel},
		{name: "missing project", id: "nope", status: model.StatusActive, wantErr: ErrNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := s.SetStatus(ctx, tc.id, tc.status)
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("SetStatus: unexpected err %v", err)
			case tc.wantErr == ErrNotFound && !errors.Is(err, ErrNotFound):
				t.Fatalf("SetStatus err = %v, want ErrNotFound", err)
			case tc.wantErr == errInvalidStatusSentinel && err == nil:
				t.Fatalf("SetStatus: expected error for invalid status")
			}
			if tc.wantErr == nil {
				got, _ := s.GetProject(ctx, tc.id)
				if got.Status != tc.status {
					t.Fatalf("status = %q, want %q", got.Status, tc.status)
				}
			}
		})
	}
}

// errInvalidStatusSentinel marks the "invalid status" case; SetStatus returns a
// dynamic (non-wrapped) error for it, so tests only assert that it is non-nil.
var errInvalidStatusSentinel = errors.New("invalid status")

func TestSettings(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.GetSetting(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSetting missing err = %v, want ErrNotFound", err)
	}

	if err := s.SetSetting(ctx, "k", "v1"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if v, _ := s.GetSetting(ctx, "k"); v != "v1" {
		t.Fatalf("GetSetting = %q, want v1", v)
	}

	// Upsert overwrites.
	if err := s.SetSetting(ctx, "k", "v2"); err != nil {
		t.Fatalf("SetSetting upsert: %v", err)
	}
	if v, _ := s.GetSetting(ctx, "k"); v != "v2" {
		t.Fatalf("GetSetting after upsert = %q, want v2", v)
	}
}
