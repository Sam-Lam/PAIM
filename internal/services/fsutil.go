package services

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/autolinepro/paim/internal/hashing"
)

// trashDirName mirrors the localfs plugin's trash convention: soft-deleted files
// are moved into "<root>/.paim-trash/" (timestamp-prefixed) rather than removed,
// so nothing is ever irreversibly lost.
const trashDirName = ".paim-trash"

// sameVolume reports whether two paths live on the same filesystem device, so a
// move can be a cheap atomic rename rather than a copy across volumes. It stats
// the containing directory of each path (the file at dst may not exist yet).
func sameVolume(a, b string) (bool, error) {
	da, err := deviceOf(a)
	if err != nil {
		return false, err
	}
	db, err := deviceOf(b)
	if err != nil {
		return false, err
	}
	return da == db, nil
}

// deviceOf returns the filesystem device ID of the directory containing path
// (falling back to path itself when the directory cannot be stat'd).
func deviceOf(path string) (uint64, error) {
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		info, err = os.Stat(path)
		if err != nil {
			return 0, fmt.Errorf("services: stat %q for device id: %w", path, err)
		}
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("services: unsupported stat type for %q", path)
	}
	return uint64(st.Dev), nil
}

// trashFile moves path into rootDir/.paim-trash/<timestamp>-<base>, creating the
// trash directory if needed, and returns the trash path. It never deletes data
// irreversibly.
func trashFile(rootDir, path string) (string, error) {
	trash := filepath.Join(rootDir, trashDirName)
	if err := os.MkdirAll(trash, 0o755); err != nil {
		return "", fmt.Errorf("services: create trash %q: %w", trash, err)
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000")
	dst := filepath.Join(trash, fmt.Sprintf("%s-%s", stamp, filepath.Base(path)))
	if err := os.Rename(path, dst); err != nil {
		return "", fmt.Errorf("services: move %q to trash: %w", path, err)
	}
	return dst, nil
}

// ensureDir creates dir (and parents) if it does not already exist.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("services: create dir %q: %w", dir, err)
	}
	return nil
}

// renameFile performs an atomic same-volume rename, wrapping the error.
func renameFile(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("services: rename %q -> %q: %w", src, dst, err)
	}
	return nil
}

// copyVerify copies src to dst (via a temp file + fsync + atomic rename) and
// verifies the copy with a full BLAKE3 comparison before returning. It mirrors
// the importer/localfs data-safety protocol. The destination directory is
// created if necessary.
func copyVerify(ctx context.Context, src, dst string) error {
	srcFull, err := hashing.FullHash(ctx, src)
	if err != nil {
		return err
	}

	dstDir := filepath.Dir(dst)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return fmt.Errorf("services: create dir %q: %w", dstDir, err)
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("services: open source %q: %w", src, err)
	}
	defer in.Close()

	tmp := filepath.Join(dstDir, ".paim-partial-"+filepath.Base(dst))
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("services: create temp %q: %w", tmp, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("services: copy %q -> %q: %w", src, tmp, err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("services: fsync %q: %w", tmp, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("services: close %q: %w", tmp, err)
	}

	dstFull, err := hashing.FullHash(ctx, tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if dstFull != srcFull {
		_ = os.Remove(tmp)
		return fmt.Errorf("services: verification failed copying %q", src)
	}

	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("services: publish %q -> %q: %w", tmp, dst, err)
	}
	return nil
}
