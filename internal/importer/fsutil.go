package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/autolinepro/paim/internal/archive"
	"github.com/google/uuid"
)

// copyBufferSize is the chunk size used by copyToPartial. Copying in chunks lets
// the copy honor context cancellation promptly.
const copyBufferSize = 1 << 20 // 1 MiB

// errDestinationFull wraps ENOSPC on the destination; it aborts the session.
var errDestinationFull = errors.New("destination disk is full")

// isNoSpace reports whether err is (or wraps) an ENOSPC condition.
func isNoSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}

// copyToPartial copies src into destDir under a unique ".paim-partial-<uuid>"
// name, fsyncing both the file and the directory, then returns the partial
// path. Copying proceeds in chunks so ctx cancellation is honored promptly; on
// cancellation (or any error) the partial file is removed and the error
// returned. onBytes, if non-nil, is called with the incremental byte count as
// the copy progresses.
func (p *Pipeline) copyToPartial(ctx context.Context, src, destDir string, onBytes func(int64)) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("copy: create dest dir %q: %w", destDir, err)
	}
	partialPath := filepath.Join(destDir, partialPrefix+uuid.NewString())

	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("copy: open source %q: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(partialPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return "", fmt.Errorf("copy: create partial %q: %w", partialPath, err)
	}

	buf := make([]byte, copyBufferSize)
	copyErr := func() error {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, rerr := in.Read(buf)
			if n > 0 {
				if _, werr := out.Write(buf[:n]); werr != nil {
					if isNoSpace(werr) {
						return fmt.Errorf("%w: %v", errDestinationFull, werr)
					}
					return fmt.Errorf("copy: write %q: %w", partialPath, werr)
				}
				if onBytes != nil {
					onBytes(int64(n))
				}
			}
			if rerr == io.EOF {
				return nil
			}
			if rerr != nil {
				return fmt.Errorf("copy: read %q: %w", src, rerr)
			}
		}
	}()

	if copyErr == nil {
		if err := out.Sync(); err != nil {
			copyErr = fmt.Errorf("copy: fsync partial %q: %w", partialPath, err)
		}
	}
	if cerr := out.Close(); cerr != nil && copyErr == nil {
		copyErr = fmt.Errorf("copy: close partial %q: %w", partialPath, cerr)
	}
	if copyErr == nil {
		if err := fsyncDir(destDir); err != nil {
			copyErr = err
		}
	}
	if copyErr != nil {
		_ = os.Remove(partialPath)
		return "", copyErr
	}
	return partialPath, nil
}

// fsyncDir fsyncs a directory so a rename/create within it is durable.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("copy: open dir for fsync %q: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("copy: fsync dir %q: %w", dir, err)
	}
	return nil
}

// deviceID returns the filesystem device ID for path, following symlinks. It is
// used to guarantee a rename stays on one volume (adopt+reorganize).
func deviceID(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("copy: cannot read device id of %q", path)
	}
	return uint64(st.Dev), nil
}

// sameVolume reports whether srcPath and the destination directory live on the
// same filesystem device. destDir may not yet exist, so its nearest existing
// ancestor is used.
func sameVolume(srcPath, destDir string) (bool, error) {
	srcDev, err := deviceID(srcPath)
	if err != nil {
		return false, fmt.Errorf("copy: stat source for volume check %q: %w", srcPath, err)
	}
	existing := destDir
	for {
		if _, err := os.Stat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		existing = parent
	}
	dstDev, err := deviceID(existing)
	if err != nil {
		return false, fmt.Errorf("copy: stat dest for volume check %q: %w", existing, err)
	}
	return srcDev == dstDev, nil
}

// computeDestination returns the absolute destination path for fi given a
// capture date, event name and layout. RAW files are routed into the RAW/
// subfolder by the layout.
func computeDestination(lay *archive.Layout, captureDate time.Time, event string, fi FileInfo) string {
	rel := lay.DestinationFor(captureDate, event, filepath.Base(fi.Path), fi.Kind)
	return filepath.Join(lay.MasterRoot, rel)
}
