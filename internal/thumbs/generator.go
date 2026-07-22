package thumbs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// qlGenerator renders thumbnails with the macOS QuickLook CLI (qlmanage) and
// compresses them to JPEG with sips. QuickLook natively handles HEIC, every
// supported RAW format, and video poster frames, which is why it is preferred
// over an in-process decoder.
type qlGenerator struct{}

// generate produces a JPEG thumbnail of srcPath at sizePx into dstPath.
//
// qlmanage quirks handled here:
//   - It writes "<outdir>/<basename>.png" where <basename> is the source file's
//     full name INCLUDING its extension (e.g. "IMG_1.heic" -> "IMG_1.heic.png"),
//     but it also rewrites some characters (notably "/" is impossible in a base
//     name, and it collapses others), so we do not trust the derived name blindly:
//     if the expected file is absent we fall back to the single PNG it left in the
//     otherwise-empty output directory.
//   - It frequently exits NON-ZERO even on success and, conversely, can exit zero
//     having produced nothing. The exit code is therefore treated as advisory; the
//     authority is whether an output PNG exists.
func (qlGenerator) generate(ctx context.Context, srcPath, dstPath string, sizePx, quality int) error {
	outDir, err := os.MkdirTemp("", "paim-ql-")
	if err != nil {
		return fmt.Errorf("thumbs: temp dir: %w", err)
	}
	defer os.RemoveAll(outDir)

	// -t: thumbnail mode, -s: max pixel size (long edge), -o: output directory.
	cmd := exec.CommandContext(ctx, "qlmanage", "-t", "-s", strconv.Itoa(sizePx), "-o", outDir, srcPath)
	out, runErr := cmd.CombinedOutput()

	png := filepath.Join(outDir, filepath.Base(srcPath)+".png")
	if !fileExists(png) {
		// Fall back to whatever single PNG qlmanage actually produced.
		png = firstPNG(outDir)
	}
	if png == "" {
		// No thumbnail at all: report the run error/output for diagnostics. This is
		// expected for genuinely unrenderable files and becomes a negative marker.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("thumbs: qlmanage produced no thumbnail for %q (err=%v): %s", srcPath, runErr, trim(out))
	}

	// Compress/convert the PNG poster frame to JPEG at the target quality, writing
	// straight to dstPath (the caller stages this in a temp file and renames it into
	// the cache atomically).
	sips := exec.CommandContext(ctx, "sips",
		"-s", "format", "jpeg",
		"-s", "formatOptions", strconv.Itoa(quality),
		png, "--out", dstPath)
	if sipsOut, err := sips.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("thumbs: sips convert %q: %v: %s", srcPath, err, trim(sipsOut))
	}
	if !fileExists(dstPath) {
		return fmt.Errorf("thumbs: sips wrote no output for %q", srcPath)
	}
	return nil
}

// firstPNG returns the path of the first *.png in dir, or "" if none exists.
// qlmanage writes exactly one PNG per thumbnail request, so "first" is
// unambiguous.
func firstPNG(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".png" {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

// trim caps captured subprocess output so a verbose failure does not bloat logs.
func trim(b []byte) string {
	const max = 300
	s := string(b)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
