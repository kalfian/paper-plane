package sitefs

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// ExtractZip reads the zip from r (of the given byte size) and extracts its
// regular files into the site, preserving directory structure. It enforces:
//
//   - zip-slip guard: every entry name is resolved via safeJoin against the
//     site root; absolute paths and ".." traversal are rejected.
//   - entry-count limit: reject archives with more than f.maxEntries entries.
//   - total-size limit: the sum of extracted (uncompressed) bytes must not
//     exceed f.maxBytes; this is enforced while copying, so a lying header
//     cannot bypass it.
//   - no symlinks/irregular files: only regular files and directories.
//
// Directory entries are used only to build structure; empty directories are
// created but not counted as written files. It returns the number of files
// written.
func (f *FS) ExtractZip(id string, r io.ReaderAt, size int64) (int, error) {
	root, err := f.siteRoot(id)
	if err != nil {
		return 0, err
	}

	zr, err := zip.NewReader(r, size)
	if err != nil {
		return 0, fmt.Errorf("sitefs: read zip: %w", err)
	}
	if len(zr.File) > f.maxEntries {
		return 0, ErrTooManyEntries
	}

	var (
		written   int
		totalWrit int64
	)
	for _, zf := range zr.File {
		// Reject symlinks and any non-regular, non-directory entry.
		mode := zf.Mode()
		if mode&fs.ModeSymlink != 0 {
			return written, ErrUnsafeEntry
		}

		// Resolve destination with the zip-slip guard. Zip names always use
		// forward slashes; safeJoin normalizes and rejects escapes.
		dest, jerr := safeJoin(root, zf.Name)
		if jerr != nil {
			return written, ErrUnsafeEntry
		}

		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return written, err
			}
			continue
		}
		if !mode.IsRegular() {
			return written, ErrUnsafeEntry
		}

		remaining := f.maxBytes - totalWrit
		if remaining < 0 {
			return written, ErrTooLarge
		}
		n, werr := f.extractOne(zf, dest, remaining)
		totalWrit += n
		if werr != nil {
			return written, werr
		}
		written++
	}
	return written, nil
}

// extractOne writes a single regular zip entry to dest, copying at most
// maxWrite bytes; if the entry is larger it returns ErrTooLarge. It returns the
// number of bytes written.
func (f *FS) extractOne(zf *zip.File, dest string, maxWrite int64) (int64, error) {
	if err := mkdirForEntry(dest); err != nil {
		return 0, err
	}
	rc, err := zf.Open()
	if err != nil {
		return 0, fmt.Errorf("sitefs: open zip entry: %w", err)
	}
	defer func() { _ = rc.Close() }()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	// Copy at most maxWrite+1 bytes: if we manage to read more than maxWrite,
	// the running total has been exceeded.
	n, cerr := io.Copy(out, io.LimitReader(rc, maxWrite+1))
	closeErr := out.Close()
	if cerr != nil {
		return n, cerr
	}
	if closeErr != nil {
		return n, closeErr
	}
	if n > maxWrite {
		_ = os.Remove(dest)
		return n, ErrTooLarge
	}
	return n, nil
}
