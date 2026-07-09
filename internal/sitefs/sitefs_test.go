package sitefs

import (
	"archive/zip"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func newTestFS(t *testing.T) *FS {
	t.Helper()
	return New(t.TempDir())
}

func TestSafeJoinRejectsTraversal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "site")
	cases := []struct {
		name    string
		relpath string
		wantErr bool
	}{
		{"simple file", "index.html", false},
		{"nested file", "assets/app.js", false},
		{"root itself", "", false},
		{"dot-current", "./index.html", false},
		{"parent escape", "../secret", true},
		{"deep parent escape", "a/../../secret", true},
		{"absolute path", "/etc/passwd", true},
		{"embedded traversal", "assets/../../etc/passwd", true},
		{"nul byte", "a\x00b", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := safeJoin(root, tc.relpath)
			if tc.wantErr {
				if !errors.Is(err, ErrUnsafePath) {
					t.Fatalf("safeJoin(%q) err = %v, want ErrUnsafePath", tc.relpath, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("safeJoin(%q) unexpected err = %v", tc.relpath, err)
			}
			cleanRoot := filepath.Clean(root)
			if got != cleanRoot && !bytes.HasPrefix([]byte(got), []byte(cleanRoot+string(os.PathSeparator))) {
				t.Fatalf("safeJoin(%q) = %q escaped root %q", tc.relpath, got, cleanRoot)
			}
		})
	}
}

func TestFileOpsRejectTraversal(t *testing.T) {
	f := newTestFS(t)
	const id = "proj1"
	if err := f.CreateSite(id); err != nil {
		t.Fatalf("CreateSite: %v", err)
	}

	if err := f.WriteFile(id, "../escape.txt", []byte("x")); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("WriteFile traversal err = %v, want ErrUnsafePath", err)
	}
	if _, err := f.ReadFile(id, "../../etc/passwd"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("ReadFile traversal err = %v, want ErrUnsafePath", err)
	}
	if err := f.DeleteFile(id, "../x"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("DeleteFile traversal err = %v, want ErrUnsafePath", err)
	}
	// Empty relpath must be rejected for file operations (would target root).
	if err := f.WriteFile(id, "", []byte("x")); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("WriteFile empty relpath err = %v, want ErrUnsafePath", err)
	}
}

// TestFileOpsRejectSiteRoot guards against relpaths that clean down to the site
// root (e.g. ".", "foo/.."). Such paths must never let a file operation read,
// overwrite, or delete the site directory itself.
func TestFileOpsRejectSiteRoot(t *testing.T) {
	f := newTestFS(t)
	const id = "proj1"
	if err := f.CreateSite(id); err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	// Seed a real file so the site dir is non-empty; if a root op ever slipped
	// through as os.Remove(root), it would fail loudly (dir not empty) — but the
	// operations must be rejected before touching disk regardless.
	if err := f.WriteFile(id, "index.html", []byte("hi")); err != nil {
		t.Fatalf("WriteFile seed: %v", err)
	}

	rootPaths := []string{"", ".", "a/..", "./", "foo/bar/../.."}
	for _, rp := range rootPaths {
		if err := f.DeleteFile(id, rp); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("DeleteFile(%q) err = %v, want ErrUnsafePath", rp, err)
		}
		if err := f.WriteFile(id, rp, []byte("x")); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("WriteFile(%q) err = %v, want ErrUnsafePath", rp, err)
		}
		if _, err := f.ReadFile(id, rp); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("ReadFile(%q) err = %v, want ErrUnsafePath", rp, err)
		}
	}

	// The site directory and its file must be untouched by the rejected ops.
	if _, err := f.ReadFile(id, "index.html"); err != nil {
		t.Fatalf("seed file gone after rejected root ops: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.sitesRoot, id)); err != nil {
		t.Fatalf("site root removed by rejected op: %v", err)
	}

	// Sanity: normal file operations still work after the guard.
	if err := f.WriteFile(id, "page.html", []byte("ok")); err != nil {
		t.Fatalf("normal WriteFile rejected: %v", err)
	}
	if got, err := f.ReadFile(id, "page.html"); err != nil || string(got) != "ok" {
		t.Fatalf("normal ReadFile = %q, %v", got, err)
	}
	if err := f.DeleteFile(id, "page.html"); err != nil {
		t.Fatalf("normal DeleteFile rejected: %v", err)
	}
}

func TestBadIDRejected(t *testing.T) {
	f := newTestFS(t)
	for _, id := range []string{"", "a/b", "..", "a..b/../.."} {
		if err := f.CreateSite(id); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("CreateSite(%q) err = %v, want ErrUnsafePath", id, err)
		}
	}
}

func TestWriteReadListDelete(t *testing.T) {
	f := newTestFS(t)
	const id = "site"
	if err := f.CreateSite(id); err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if err := f.WriteFile(id, "index.html", []byte("hello")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := f.WriteFile(id, "css/app.css", []byte("body{}")); err != nil {
		t.Fatalf("WriteFile nested: %v", err)
	}

	got, err := f.ReadFile(id, "index.html")
	if err != nil || string(got) != "hello" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}

	files, err := f.ListFiles(id)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("ListFiles len = %d, want 2 (%+v)", len(files), files)
	}
	// Nested path uses forward slashes.
	var foundNested bool
	for _, fi := range files {
		if fi.Path == "css/app.css" {
			foundNested = true
		}
	}
	if !foundNested {
		t.Fatalf("ListFiles missing css/app.css: %+v", files)
	}

	size, err := f.SiteSize(id)
	if err != nil {
		t.Fatalf("SiteSize: %v", err)
	}
	if size != int64(len("hello")+len("body{}")) {
		t.Fatalf("SiteSize = %d", size)
	}

	if err := f.DeleteFile(id, "index.html"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := f.ReadFile(id, "index.html"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadFile after delete err = %v, want ErrNotExist", err)
	}
}

func TestPlaceholderAndDeleteSite(t *testing.T) {
	f := newTestFS(t)
	const id = "ph"
	if err := f.CreateSite(id); err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if err := f.WritePlaceholderIndex(id); err != nil {
		t.Fatalf("WritePlaceholderIndex: %v", err)
	}
	data, err := f.ReadFile(id, "index.html")
	if err != nil {
		t.Fatalf("ReadFile placeholder: %v", err)
	}
	if !bytes.Contains(data, []byte("static site kosong")) {
		t.Fatalf("placeholder missing expected text: %s", data)
	}

	if err := f.DeleteSite(id); err != nil {
		t.Fatalf("DeleteSite: %v", err)
	}
	if _, err := f.Stat(id, "index.html"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat after DeleteSite err = %v, want ErrNotExist", err)
	}
}

// zipEntry is a small helper describing an entry for buildZip.
type zipEntry struct {
	name string
	body string
	dir  bool
}

func buildZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		name := e.name
		if e.dir && name[len(name)-1] != '/' {
			name += "/"
		}
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if !e.dir {
			if _, err := w.Write([]byte(e.body)); err != nil {
				t.Fatalf("zip write: %v", err)
			}
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractZipHappyPath(t *testing.T) {
	f := newTestFS(t)
	const id = "z"
	if err := f.CreateSite(id); err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	data := buildZip(t, []zipEntry{
		{name: "index.html", body: "<h1>hi</h1>"},
		{name: "assets/", dir: true},
		{name: "assets/app.js", body: "console.log(1)"},
	})
	n, err := f.ExtractZip(id, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("ExtractZip: %v", err)
	}
	if n != 2 {
		t.Fatalf("ExtractZip wrote %d files, want 2", n)
	}
	got, err := f.ReadFile(id, "assets/app.js")
	if err != nil || string(got) != "console.log(1)" {
		t.Fatalf("extracted file = %q, %v", got, err)
	}
}

func TestExtractZipRejectsZipSlip(t *testing.T) {
	f := newTestFS(t)
	const id = "z"
	if err := f.CreateSite(id); err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	for _, name := range []string{"../evil.txt", "a/../../evil.txt", "/etc/evil"} {
		data := buildZip(t, []zipEntry{{name: name, body: "pwn"}})
		if _, err := f.ExtractZip(id, bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrUnsafeEntry) {
			t.Fatalf("ExtractZip(%q) err = %v, want ErrUnsafeEntry", name, err)
		}
	}
	// Ensure nothing escaped to the parent of the sites root.
	escaped := filepath.Join(f.sitesRoot, "..", "evil.txt")
	if _, err := os.Stat(escaped); err == nil {
		t.Fatalf("zip-slip wrote escaped file at %s", escaped)
	}
}

func TestExtractZipEntryLimit(t *testing.T) {
	f := newTestFS(t)
	f.maxEntries = 2
	const id = "z"
	_ = f.CreateSite(id)
	data := buildZip(t, []zipEntry{
		{name: "a.txt", body: "a"},
		{name: "b.txt", body: "b"},
		{name: "c.txt", body: "c"},
	})
	if _, err := f.ExtractZip(id, bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrTooManyEntries) {
		t.Fatalf("ExtractZip err = %v, want ErrTooManyEntries", err)
	}
}

func TestExtractZipSizeLimit(t *testing.T) {
	f := newTestFS(t)
	f.maxBytes = 10 // 10 bytes total budget
	const id = "z"
	_ = f.CreateSite(id)
	data := buildZip(t, []zipEntry{{name: "big.txt", body: "0123456789ABCDEF"}})
	if _, err := f.ExtractZip(id, bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("ExtractZip err = %v, want ErrTooLarge", err)
	}
}
