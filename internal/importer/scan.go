package importer

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/autolinepro/paim/internal/mediatype"
)

// FileInfo is a single media file discovered during a scan.
type FileInfo struct {
	Path    string // absolute path
	Size    int64  // bytes
	Ext     string // lowercase, no leading dot
	ModTime int64  // unix seconds of the file mtime
	Kind    mediatype.Kind
}

// ProvisionalPair links a still to a companion MOV by shared directory+basename,
// before ContentIdentifier is known. It is reconciled at import time (see the
// package documentation on two-stage pairing).
type ProvisionalPair struct {
	StillPath  string
	MotionPath string
}

// ScanResult is the output of Scan: every media file found under Root plus the
// provisional Live Photo pairs and aggregate totals.
type ScanResult struct {
	Root             string
	Files            []FileInfo
	ProvisionalPairs []ProvisionalPair
	TotalBytes       int64
}

// skipDirNames are directory basenames that are always skipped: macOS system
// folders and camera thumbnail/junk directories that never hold importable
// originals.
var skipDirNames = map[string]bool{
	".Trashes":                true,
	".Spotlight-V100":         true,
	".fseventsd":              true,
	".DocumentRevisions-V100": true,
	".TemporaryItems":         true,
	"THMBNL":                  true,
	"MISC":                    true,
}

// isHidden reports whether a path component is hidden (a dotfile). The root
// component itself is never treated as hidden.
func isHidden(name string) bool {
	return strings.HasPrefix(name, ".")
}

// Scan walks root recursively, skipping hidden files/dirs and camera/system junk
// directories, classifies each remaining file via mediatype, and collects a
// FileInfo for every supported media file. progressFn (may be nil) is called
// periodically with a running file count. Provisional Live Photo pairs are
// grouped by directory+basename; they are reconciled against ContentIdentifier
// at import time.
func (p *Pipeline) Scan(ctx context.Context, root string, progressFn ProgressFunc) (*ScanResult, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("scan: resolve root %q: %w", root, err)
	}

	res := &ScanResult{Root: absRoot}
	count := 0

	walkErr := filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A single unreadable entry must not abort the whole scan; log and skip.
			p.log.Warn("scan: skipping unreadable entry", "path", path, "error", err.Error())
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		name := d.Name()
		if d.IsDir() {
			// Never skip the root itself even if its name looks hidden.
			if path == absRoot {
				return nil
			}
			if isHidden(name) || skipDirNames[name] {
				return fs.SkipDir
			}
			return nil
		}

		if isHidden(name) {
			return nil
		}
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
		if !mediatype.IsMedia(ext) {
			return nil
		}

		info, statErr := d.Info()
		if statErr != nil {
			p.log.Warn("scan: cannot stat file", "path", path, "error", statErr.Error())
			return nil
		}

		fi := FileInfo{
			Path:    path,
			Size:    info.Size(),
			Ext:     ext,
			ModTime: info.ModTime().Unix(),
			Kind:    mediatype.KindOf(ext),
		}
		res.Files = append(res.Files, fi)
		res.TotalBytes += fi.Size
		count++
		if count%64 == 0 {
			progressFn.emit(Progress{Phase: PhaseScanning, FilesDone: count, CurrentFile: path})
		}
		return nil
	})
	if walkErr != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("scan %q: %w", absRoot, ctx.Err())
		}
		return nil, fmt.Errorf("scan %q: %w", absRoot, walkErr)
	}

	res.ProvisionalPairs = provisionalPairs(res.Files)
	progressFn.emit(Progress{Phase: PhaseScanning, FilesDone: count, FilesTotal: count})
	p.log.Info("scan complete", "root", absRoot, "files", len(res.Files), "bytes", res.TotalBytes)
	return res, nil
}

// provisionalPairs groups files by directory+basename and returns a pair for
// each still (HEIC/JPG) that has exactly one companion MOV sharing its key. This
// is the coarse, ContentIdentifier-unaware first stage of Live Photo pairing.
func provisionalPairs(files []FileInfo) []ProvisionalPair {
	type key struct{ dir, base string }
	stills := map[key][]string{}
	motions := map[key][]string{}

	for _, f := range files {
		k := key{dir: filepath.Dir(f.Path), base: baseNoExt(f.Path)}
		switch f.Ext {
		case "heic", "jpg", "jpeg":
			stills[k] = append(stills[k], f.Path)
		case "mov":
			motions[k] = append(motions[k], f.Path)
		}
	}

	var pairs []ProvisionalPair
	for _, f := range files {
		if f.Ext != "heic" && f.Ext != "jpg" && f.Ext != "jpeg" {
			continue
		}
		k := key{dir: filepath.Dir(f.Path), base: baseNoExt(f.Path)}
		if len(stills[k]) != 1 || len(motions[k]) != 1 {
			continue
		}
		if stills[k][0] != f.Path {
			continue
		}
		pairs = append(pairs, ProvisionalPair{StillPath: f.Path, MotionPath: motions[k][0]})
	}
	return pairs
}

func baseNoExt(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// statSize returns the current on-disk size of path, used to validate a reused
// precomputed hash and to detect a source file that vanished mid-run.
func statSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
