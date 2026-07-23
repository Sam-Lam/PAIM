package thumbs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/Sam-Lam/PAIM/internal/mediatype"
)

// qlGenerator renders thumbnails with the macOS QuickLook CLI (qlmanage) and
// compresses them to JPEG with sips. QuickLook natively handles HEIC, every
// supported RAW format, and video poster frames, which is why it is preferred
// over an in-process decoder.
//
// RAW files take a faster, more reliable path first: cameras embed one or more
// full JPEG previews in every RAW container, so we extract the largest embedded
// preview with exiftool and feed THAT to sips, skipping QuickLook's RAW decoder
// entirely. Embedded-preview extraction is camera-agnostic (no per-format
// decoder), is far cheaper than decoding the RAW mosaic, and sidesteps the cases
// where qlmanage silently produces nothing for a RAW. Only when no preview can be
// extracted (exiftool absent, or a RAW with no embedded JPEG) do RAW files fall
// back to the qlmanage path unchanged.
//
// The runExif, exiftoolPath, and qlRender fields are injection seams for tests;
// production code uses newQLGenerator to wire the real subprocess implementations.
type qlGenerator struct {
	// runExif runs an exiftool-style command and returns its stdout. Defaults to
	// execExif when nil.
	runExif func(ctx context.Context, name string, args ...string) ([]byte, error)
	// exiftoolPath resolves the exiftool binary, returning "" when none is usable
	// (disabling embedded-preview extraction). Defaults to lookExiftool when nil.
	exiftoolPath func() string
	// qlRender renders srcPath via the qlmanage+sips pipeline. Defaults to
	// quickLookRender when nil; tests stub it so they never spawn qlmanage (which
	// can block indefinitely on an unrenderable/missing source).
	qlRender func(ctx context.Context, srcPath, dstPath string, sizePx, quality int) error
}

// newQLGenerator constructs the production generator wired to the real exiftool
// runner, binary discovery, and qlmanage pipeline.
func newQLGenerator() qlGenerator {
	return qlGenerator{runExif: execExif, exiftoolPath: lookExiftool, qlRender: quickLookRender}
}

// generate produces a JPEG thumbnail of srcPath at sizePx into dstPath.
//
// RAW sources try embedded-preview extraction first (see the type doc); on any
// miss they fall through to the qlmanage pipeline below.
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
func (g qlGenerator) generate(ctx context.Context, srcPath, dstPath string, sizePx, quality int) error {
	// RAW: prefer the embedded JPEG preview. It is already a JPEG, so it goes
	// straight to sips (with a resize, since a full-resolution preview must be
	// bounded to sizePx) and never touches qlmanage.
	if mediatype.KindOf(filepath.Ext(srcPath)) == mediatype.RawPhoto {
		if preview, ok := g.rawPreview(ctx, srcPath); ok {
			defer os.Remove(preview)
			return sipsConvert(ctx, preview, dstPath, sizePx, quality, true)
		}
		// No embedded preview (or no exiftool): fall through to qlmanage unchanged.
	}

	render := g.qlRender
	if render == nil {
		render = quickLookRender
	}
	return render(ctx, srcPath, dstPath, sizePx, quality)
}

// quickLookRender is the qlmanage+sips pipeline: it renders a poster PNG for any
// still, RAW, or video via QuickLook, then compresses it to JPEG with sips.
func quickLookRender(ctx context.Context, srcPath, dstPath string, sizePx, quality int) error {
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
	// the cache atomically). qlmanage already sized the poster to sizePx, so no
	// resize is requested here.
	return sipsConvert(ctx, png, dstPath, sizePx, quality, false)
}

// rawPreviewTags are the exiftool binary tags tried, in order, when extracting an
// embedded JPEG preview from a RAW file. PreviewImage is typically the largest
// (often full-resolution) embedded JPEG; JpgFromRaw is present in some Canon/Nikon
// RAWs; ThumbnailImage is a small last resort.
var rawPreviewTags = []string{"PreviewImage", "JpgFromRaw", "ThumbnailImage"}

// rawPreview extracts the largest available embedded JPEG preview from a RAW file
// into a temp file, returning its path and true on success. It returns ok=false
// when exiftool is unavailable or the RAW carries no embedded JPEG, so the caller
// can fall back to qlmanage. The returned temp file is the caller's to remove.
//
// It shares its extraction slot/ctx with the enclosing generation, so cancellation
// stops the tag walk promptly and no unbounded extra work is started.
func (g qlGenerator) rawPreview(ctx context.Context, srcPath string) (string, bool) {
	resolve := g.exiftoolPath
	if resolve == nil {
		resolve = lookExiftool
	}
	bin := resolve()
	if bin == "" {
		return "", false // no exiftool: caller falls back to qlmanage
	}
	run := g.runExif
	if run == nil {
		run = execExif
	}

	for _, tag := range rawPreviewTags {
		if ctx.Err() != nil {
			return "", false
		}
		// -b writes the raw binary tag value (the embedded JPEG) to stdout.
		out, err := run(ctx, bin, "-b", "-"+tag, srcPath)
		if err != nil || !isJPEG(out) {
			continue // absent/empty tag, or non-JPEG payload: try the next one
		}
		tmp, err := os.CreateTemp("", "paim-rawprev-*.jpg")
		if err != nil {
			return "", false
		}
		path := tmp.Name()
		_, wErr := tmp.Write(out)
		cErr := tmp.Close()
		if wErr != nil || cErr != nil {
			_ = os.Remove(path)
			return "", false
		}
		return path, true
	}
	return "", false
}

// sipsConvert compresses src to a JPEG at dst using sips. When resize is true the
// long edge is bounded to sizePx (-Z); the qlmanage path passes resize=false since
// qlmanage already sized its poster, while the RAW-preview path passes true to
// bound an otherwise full-resolution embedded preview.
func sipsConvert(ctx context.Context, src, dst string, sizePx, quality int, resize bool) error {
	args := []string{"-s", "format", "jpeg", "-s", "formatOptions", strconv.Itoa(quality)}
	if resize {
		args = append(args, "-Z", strconv.Itoa(sizePx))
	}
	args = append(args, src, "--out", dst)

	sips := exec.CommandContext(ctx, "sips", args...)
	if out, err := sips.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("thumbs: sips convert %q: %v: %s", src, err, trim(out))
	}
	if !fileExists(dst) {
		return fmt.Errorf("thumbs: sips wrote no output for %q", src)
	}
	return nil
}

// execExif is the production exiftool runner: it runs the command and returns its
// stdout only (stderr is dropped; a non-zero exit is reported as err).
func execExif(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// isJPEG reports whether b begins with the JPEG SOI marker.
func isJPEG(b []byte) bool {
	return len(b) >= 2 && b[0] == 0xFF && b[1] == 0xD8
}

// defaultExiftoolPath is the Homebrew fallback used when exiftool is not on PATH;
// it mirrors internal/metadata's discovery so both subsystems find the same binary.
const defaultExiftoolPath = "/opt/homebrew/bin/exiftool"

// lookExiftool resolves the exiftool binary, preferring PATH and falling back to
// the known Homebrew location. It returns "" when no usable binary is found, which
// disables embedded-preview extraction (RAW files then use qlmanage).
func lookExiftool() string {
	if p, err := exec.LookPath("exiftool"); err == nil {
		return p
	}
	if _, err := os.Stat(defaultExiftoolPath); err == nil {
		return defaultExiftoolPath
	}
	return ""
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
