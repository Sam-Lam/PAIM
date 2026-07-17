// Package localfs implements the first PAIM backup plugin: a local (or mounted)
// filesystem destination. It copies verified assets into a configured root
// directory using the same data-safety protocol as the importer — write to a
// temporary ".paim-partial-<uuid>" file, fsync the file and its parent
// directory, then atomically rename into place — and verifies copies with a full
// BLAKE3 comparison.
//
// Deletes are never destructive: a "deleted" object is moved into a
// "<root>/.paim-trash/" folder (timestamp-prefixed) rather than removed, honoring
// PAIM's data-integrity ethos. Recovery is a manual move back out of trash.
//
// The plugin is registered with a backup.Registry by main.go; the backup core
// never imports it directly.
package localfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/autolinepro/paim/internal/backup"
	"github.com/autolinepro/paim/internal/hashing"
	"github.com/google/uuid"
)

// PluginName is the stable identifier under which this plugin registers.
const PluginName = "localfs"

// trashDir is the subdirectory of the root into which deletes are moved.
const trashDir = ".paim-trash"

// copyBufferSize is the chunk size used while copying (1 MiB). Context is checked
// between chunks and progress is reported per chunk.
const copyBufferSize = 1 << 20

// Config is the JSON configuration for a localfs provider: {"root": "/path"}.
type Config struct {
	Root string `json:"root"`
}

// Plugin is a filesystem-backed backup.Plugin.
type Plugin struct {
	root string
}

// New returns an unconfigured localfs plugin. It is the backup.Factory for this
// plugin; register it with reg.Register(localfs.PluginName, localfs.New).
func New() backup.Plugin { return &Plugin{} }

var _ backup.Plugin = (*Plugin)(nil)

// Name returns the plugin identifier.
func (p *Plugin) Name() string { return PluginName }

// Initialize parses the config, resolves the root to an absolute path, and
// verifies the root exists and is writable by probing it with a temporary file.
func (p *Plugin) Initialize(ctx context.Context, configJSON string) error {
	var cfg Config
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("localfs: parse config: %w", err)
	}
	if strings.TrimSpace(cfg.Root) == "" {
		return errors.New("localfs: config root is required")
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return fmt.Errorf("localfs: resolve root %q: %w", cfg.Root, err)
	}

	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("localfs: stat root %q: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("localfs: root %q is not a directory", root)
	}

	// Probe writability with a temporary file rather than trusting mode bits.
	probe := filepath.Join(root, fmt.Sprintf(".paim-probe-%s", uuid.NewString()))
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("localfs: root %q is not writable: %w", root, err)
	}
	_ = f.Close()
	_ = os.Remove(probe)

	p.root = root
	return nil
}

// Authenticate is a no-op: a local filesystem needs no credentials.
func (p *Plugin) Authenticate(ctx context.Context) error { return nil }

// Capabilities reports that localfs verifies and (soft-)deletes but does not
// resume interrupted uploads (a partial temp file is discarded and re-copied),
// and imposes no maximum file size.
func (p *Plugin) Capabilities() backup.Capabilities {
	return backup.Capabilities{
		SupportsVerify: true,
		SupportsDelete: true,
		SupportsResume: false,
		MaxFileSize:    0,
	}
}

// Upload copies localPath to <root>/<remoteRelPath> using the temp-file + fsync +
// atomic-rename protocol. A partially written temp file is removed if the copy
// fails or the context is cancelled, so an interrupted upload never leaves a
// visible destination file.
func (p *Plugin) Upload(ctx context.Context, localPath, remoteRelPath string, progressFn func(bytesDone, bytesTotal int64)) error {
	if p.root == "" {
		return errors.New("localfs: plugin not initialized")
	}

	dst, err := p.resolve(remoteRelPath)
	if err != nil {
		return err
	}

	src, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("localfs: open source %q: %w", localPath, err)
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return fmt.Errorf("localfs: stat source %q: %w", localPath, err)
	}
	total := info.Size()

	dstDir := filepath.Dir(dst)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return fmt.Errorf("localfs: create destination dir %q: %w", dstDir, err)
	}

	tmp := filepath.Join(dstDir, fmt.Sprintf(".paim-partial-%s", uuid.NewString()))
	if err := p.copyToTemp(ctx, src, tmp, total, progressFn); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	// Atomic publish: rename temp into place, then fsync the directory so the
	// rename is durable.
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("localfs: rename %q -> %q: %w", tmp, dst, err)
	}
	if err := fsyncDir(dstDir); err != nil {
		return fmt.Errorf("localfs: fsync dir %q: %w", dstDir, err)
	}
	return nil
}

// copyToTemp streams src into a freshly created temp file, checking ctx between
// chunks, reporting progress, and fsyncing the file before it returns.
func (p *Plugin) copyToTemp(ctx context.Context, src io.Reader, tmp string, total int64, progressFn func(bytesDone, bytesTotal int64)) error {
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("localfs: create temp %q: %w", tmp, err)
	}
	// Close is deferred but errors on the write path are returned explicitly
	// below; the deferred close is a best-effort backstop.
	defer out.Close()

	buf := make([]byte, copyBufferSize)
	var done int64
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("localfs: copy cancelled: %w", err)
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return fmt.Errorf("localfs: write temp %q: %w", tmp, werr)
			}
			done += int64(n)
			if progressFn != nil {
				progressFn(done, total)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("localfs: read source: %w", readErr)
		}
	}

	if err := out.Sync(); err != nil {
		return fmt.Errorf("localfs: fsync temp %q: %w", tmp, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("localfs: close temp %q: %w", tmp, err)
	}
	return nil
}

// Verify recomputes the full BLAKE3 hash of both the source and the destination
// object and reports whether they match. A missing destination returns
// (false, nil); an unreadable source or destination returns an error.
func (p *Plugin) Verify(ctx context.Context, localPath, remoteRelPath string) (bool, error) {
	dst, err := p.resolve(remoteRelPath)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(dst); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("localfs: stat destination %q: %w", dst, err)
	}

	srcHash, err := hashing.FullHash(ctx, localPath)
	if err != nil {
		return false, fmt.Errorf("localfs: hash source %q: %w", localPath, err)
	}
	dstHash, err := hashing.FullHash(ctx, dst)
	if err != nil {
		return false, fmt.Errorf("localfs: hash destination %q: %w", dst, err)
	}
	return srcHash == dstHash, nil
}

// Delete performs a soft delete: it moves <root>/<remoteRelPath> into
// <root>/.paim-trash/<timestamp>-<name>. It never removes data irreversibly. A
// missing object is treated as already deleted (no error).
func (p *Plugin) Delete(ctx context.Context, remoteRelPath string) error {
	src, err := p.resolve(remoteRelPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("localfs: stat %q: %w", src, err)
	}

	trash := filepath.Join(p.root, trashDir)
	if err := os.MkdirAll(trash, 0o755); err != nil {
		return fmt.Errorf("localfs: create trash %q: %w", trash, err)
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000")
	dst := filepath.Join(trash, fmt.Sprintf("%s-%s", stamp, filepath.Base(src)))

	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("localfs: move %q to trash: %w", src, err)
	}
	if err := fsyncDir(trash); err != nil {
		return fmt.Errorf("localfs: fsync trash %q: %w", trash, err)
	}
	return nil
}

// resolve maps a remote-relative path to an absolute path under the root,
// rejecting paths that would escape the root (via "..").
func (p *Plugin) resolve(remoteRelPath string) (string, error) {
	if p.root == "" {
		return "", errors.New("localfs: plugin not initialized")
	}
	clean := filepath.Clean("/" + filepath.FromSlash(remoteRelPath))
	dst := filepath.Join(p.root, clean)
	// After joining, ensure the result is still within root.
	rel, err := filepath.Rel(p.root, dst)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("localfs: remote path %q escapes root", remoteRelPath)
	}
	return dst, nil
}

// fsyncDir opens a directory and fsyncs it so a rename/create within it is
// durable.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return err
	}
	return nil
}
