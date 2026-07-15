// Package sitefs manages the on-disk files of each project's static site,
// rooted at DataDir/sites/<projectID>/. Every operation that accepts a
// caller-supplied relative path routes through safeJoin, which rejects path
// traversal (".."), absolute paths, and anything that would escape the site
// root. The FileStore interface keeps the boundary mockable for handler tests.
package sitefs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Default upload limits. They are struct fields on FS (see New) so tests and
// callers can override them, but these are the production defaults.
const (
	// DefaultMaxUploadBytes caps the total uncompressed bytes accepted from a
	// single upload request (including the sum of a zip's entries).
	DefaultMaxUploadBytes int64 = 50 << 20 // 50 MiB
	// DefaultMaxZipEntries caps the number of entries in a single uploaded zip.
	DefaultMaxZipEntries = 500
)

// Sentinel errors. Missing files are reported as fs.ErrNotExist so callers can
// use errors.Is(err, fs.ErrNotExist).
var (
	// ErrUnsafePath is returned when a relative path would escape the site root
	// (contains "..", is absolute, or otherwise resolves outside the root).
	ErrUnsafePath = errors.New("sitefs: unsafe path")
	// ErrTooLarge is returned when an upload exceeds the configured byte limit.
	ErrTooLarge = errors.New("sitefs: upload exceeds size limit")
	// ErrTooManyEntries is returned when a zip has more entries than allowed.
	ErrTooManyEntries = errors.New("sitefs: zip exceeds entry limit")
	// ErrUnsafeEntry is returned when a zip entry is a symlink or irregular file.
	ErrUnsafeEntry = errors.New("sitefs: unsafe zip entry")
)

// FileInfo describes a single file within a site.
type FileInfo struct {
	// Path is the slash-separated path relative to the site root.
	Path string
	// Size is the file size in bytes.
	Size int64
}

// FileStore is the persistence boundary for site files. All relative paths are
// validated; implementations must reject traversal. It is an interface so
// server handlers can be tested against a fake.
type FileStore interface {
	// CreateSite creates the (empty) directory for a project's site. It is
	// idempotent: creating an existing site is not an error.
	CreateSite(id string) error
	// DeleteSite removes a project's site directory and all of its contents.
	DeleteSite(id string) error
	// WritePlaceholderIndex writes a minimal index.html for an empty site.
	WritePlaceholderIndex(id string) error
	// ListFiles returns every file (not directories) under the site, with
	// slash-separated relative paths, sorted lexically.
	ListFiles(id string) ([]FileInfo, error)
	// ReadFile returns the contents of relpath within the site.
	ReadFile(id, relpath string) ([]byte, error)
	// WriteFile writes data to relpath within the site, creating parent
	// directories as needed.
	WriteFile(id, relpath string, data []byte) error
	// DeleteFile removes the file at relpath within the site.
	DeleteFile(id, relpath string) error
	// Rename moves the file at oldRel to newRel within the site, creating parent
	// directories as needed. Both paths are validated against traversal. It
	// returns fs.ErrNotExist if the source is missing and os.ErrExist if the
	// destination already exists (renames never overwrite).
	Rename(id, oldRel, newRel string) error
	// SiteSize returns the total size in bytes of all files under the site.
	SiteSize(id string) (int64, error)
	// Stat returns file info for relpath within the site, or fs.ErrNotExist.
	Stat(id, relpath string) (os.FileInfo, error)
	// ExtractZip extracts the zip readable from r (of the given size) into the
	// site, guarding against zip-slip, oversized/too-many entries, and
	// symlinks. It returns the number of files written.
	ExtractZip(id string, r io.ReaderAt, size int64) (int, error)
}

// FS is the filesystem-backed FileStore. Sites live under sitesRoot/<id>/.
type FS struct {
	sitesRoot   string
	maxBytes    int64
	maxEntries  int
	placeholder []byte
}

// compile-time assertion that FS satisfies FileStore.
var _ FileStore = (*FS)(nil)

// placeholderIndex is written for a freshly created, empty site.
const placeholderIndex = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>Empty site</title></head>
<body><p>static site kosong</p></body>
</html>
`

// New returns an FS storing sites under dataDir/sites with the default upload
// limits.
func New(dataDir string) *FS {
	return &FS{
		sitesRoot:   filepath.Join(dataDir, "sites"),
		maxBytes:    DefaultMaxUploadBytes,
		maxEntries:  DefaultMaxZipEntries,
		placeholder: []byte(placeholderIndex),
	}
}

// siteRoot returns the validated absolute directory for a project's site. The
// id must not contain path separators or "..", since it becomes a folder name.
func (f *FS) siteRoot(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return "", ErrUnsafePath
	}
	return filepath.Join(f.sitesRoot, id), nil
}

// safeJoin joins root and relpath, guaranteeing the result stays within root.
// It rejects absolute paths, NUL bytes, and any path that (after cleaning "..")
// escapes root. An empty relpath resolves to root itself.
func safeJoin(root, relpath string) (string, error) {
	if strings.ContainsRune(relpath, 0) {
		return "", ErrUnsafePath
	}
	// Normalize slashes for the host OS. Zip and URL paths use forward slashes.
	rp := filepath.FromSlash(relpath)
	if rp == "" {
		return filepath.Clean(root), nil
	}
	if filepath.IsAbs(rp) {
		return "", ErrUnsafePath
	}
	cleanRoot := filepath.Clean(root)
	joined := filepath.Join(cleanRoot, rp)
	// filepath.Join cleans the result, resolving any ".." segments. If the
	// result is not the root or a descendant of it, the path escaped.
	if joined != cleanRoot && !strings.HasPrefix(joined, cleanRoot+string(os.PathSeparator)) {
		return "", ErrUnsafePath
	}
	return joined, nil
}

// resolve validates both id and relpath and returns the absolute file path.
func (f *FS) resolve(id, relpath string) (string, error) {
	root, err := f.siteRoot(id)
	if err != nil {
		return "", err
	}
	return safeJoin(root, relpath)
}

// resolveFile is like resolve but additionally rejects any relpath that resolves
// to the site root itself (e.g. "", ".", "foo/.."). Per-file operations must
// target a path *within* the site, never the root directory, so a caller can
// never read, overwrite, or delete the site root via a crafted path. Violations
// return ErrUnsafePath (no internal path is leaked).
func (f *FS) resolveFile(id, relpath string) (string, error) {
	root, err := f.siteRoot(id)
	if err != nil {
		return "", err
	}
	full, err := safeJoin(root, relpath)
	if err != nil {
		return "", err
	}
	if full == filepath.Clean(root) {
		return "", ErrUnsafePath
	}
	return full, nil
}

// CreateSite creates the site directory (idempotent).
func (f *FS) CreateSite(id string) error {
	root, err := f.siteRoot(id)
	if err != nil {
		return err
	}
	return os.MkdirAll(root, 0o755)
}

// DeleteSite removes the site directory and its contents.
func (f *FS) DeleteSite(id string) error {
	root, err := f.siteRoot(id)
	if err != nil {
		return err
	}
	return os.RemoveAll(root)
}

// WritePlaceholderIndex writes the empty-site index.html.
func (f *FS) WritePlaceholderIndex(id string) error {
	return f.WriteFile(id, "index.html", f.placeholder)
}

// ListFiles walks the site directory and returns all files (not directories).
func (f *FS) ListFiles(id string) ([]FileInfo, error) {
	root, err := f.siteRoot(id)
	if err != nil {
		return nil, err
	}
	var out []FileInfo
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // site dir absent → empty listing
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, FileInfo{Path: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return out, nil
}

// ReadFile reads relpath within the site. It rejects paths that resolve to the
// site root (see resolveFile).
func (f *FS) ReadFile(id, relpath string) ([]byte, error) {
	full, err := f.resolveFile(id, relpath)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}

// WriteFile writes data to relpath, creating parent directories. It rejects
// paths that resolve to the site root (see resolveFile).
func (f *FS) WriteFile(id, relpath string, data []byte) error {
	full, err := f.resolveFile(id, relpath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, data, 0o644)
}

// DeleteFile removes the file at relpath. It rejects paths that resolve to the
// site root (see resolveFile), so it can never remove the site directory itself.
func (f *FS) DeleteFile(id, relpath string) error {
	full, err := f.resolveFile(id, relpath)
	if err != nil {
		return err
	}
	return os.Remove(full)
}

// Rename moves oldRel to newRel within the site. Both are validated against the
// site root and rejected if they resolve to the root itself. Parent directories
// for the destination are created. It refuses to overwrite an existing
// destination (os.ErrExist) and reports a missing source as fs.ErrNotExist.
func (f *FS) Rename(id, oldRel, newRel string) error {
	src, err := f.resolveFile(id, oldRel)
	if err != nil {
		return err
	}
	dst, err := f.resolveFile(id, newRel)
	if err != nil {
		return err
	}
	if src == dst {
		return nil
	}
	if _, err := os.Stat(src); err != nil {
		return err // fs.ErrNotExist when the source is missing
	}
	if _, err := os.Stat(dst); err == nil {
		return os.ErrExist
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

// SiteSize sums the sizes of all files under the site.
func (f *FS) SiteSize(id string) (int64, error) {
	files, err := f.ListFiles(id)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, fi := range files {
		total += fi.Size
	}
	return total, nil
}

// Stat returns os.FileInfo for relpath, or fs.ErrNotExist.
func (f *FS) Stat(id, relpath string) (os.FileInfo, error) {
	full, err := f.resolve(id, relpath)
	if err != nil {
		return nil, err
	}
	return os.Stat(full)
}

// mkdirForEntry creates the parent directory for the destination path.
func mkdirForEntry(dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("sitefs: mkdir: %w", err)
	}
	return nil
}
