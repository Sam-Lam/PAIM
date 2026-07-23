package thumbs

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// jpegBytes returns a minimal valid JPEG of the given size (distinct sizes yield
// distinct byte streams, so tests can assert which preview was extracted).
func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

// recordingRunner is an injectable exiftool runner. It records the tag requested
// on each call and returns canned output per tag, so the fallback chain and the
// exact command construction can be asserted without a real exiftool.
type recordingRunner struct {
	byTag map[string][]byte // tag (e.g. "PreviewImage") -> stdout bytes
	calls []string          // tags requested, in order
	bins  []string          // binary path passed on each call
}

func (r *recordingRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.bins = append(r.bins, name)
	// Args are always: -b -<Tag> <src>. Assert that shape and pull the tag.
	if len(args) != 3 || args[0] != "-b" || len(args[1]) < 2 || args[1][0] != '-' {
		return nil, nil
	}
	tag := args[1][1:]
	r.calls = append(r.calls, tag)
	return r.byTag[tag], nil
}

// TestRawPreviewFallbackChain verifies the tags are tried in order (PreviewImage,
// JpgFromRaw, ThumbnailImage) and that the first tag yielding a JPEG wins.
func TestRawPreviewFallbackChain(t *testing.T) {
	want := jpegBytes(t, 32, 24)
	rr := &recordingRunner{byTag: map[string][]byte{
		// PreviewImage and JpgFromRaw absent (nil); ThumbnailImage present.
		"ThumbnailImage": want,
	}}
	g := qlGenerator{runExif: rr.run, exiftoolPath: func() string { return "/fake/exiftool" }}

	path, ok := g.rawPreview(context.Background(), "/some/file.raf")
	if !ok {
		t.Fatal("rawPreview ok=false, want true")
	}
	defer os.Remove(path)

	// The chain must have walked all three tags in the documented order.
	wantCalls := []string{"PreviewImage", "JpgFromRaw", "ThumbnailImage"}
	if len(rr.calls) != len(wantCalls) {
		t.Fatalf("tags tried = %v, want %v", rr.calls, wantCalls)
	}
	for i := range wantCalls {
		if rr.calls[i] != wantCalls[i] {
			t.Errorf("tag[%d] = %q, want %q", i, rr.calls[i], wantCalls[i])
		}
	}
	// The resolved binary path must be forwarded to the runner.
	if len(rr.bins) == 0 || rr.bins[0] != "/fake/exiftool" {
		t.Errorf("runner bin = %v, want /fake/exiftool", rr.bins)
	}
	// The extracted temp file must hold exactly the winning tag's bytes.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read extracted preview: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("extracted %d bytes, want %d (ThumbnailImage payload)", len(got), len(want))
	}
}

// TestRawPreviewFirstTagWins verifies extraction stops at the first tag that
// yields a JPEG (PreviewImage), never trying the later tags.
func TestRawPreviewFirstTagWins(t *testing.T) {
	rr := &recordingRunner{byTag: map[string][]byte{
		"PreviewImage":   jpegBytes(t, 64, 48),
		"ThumbnailImage": jpegBytes(t, 8, 8),
	}}
	g := qlGenerator{runExif: rr.run, exiftoolPath: func() string { return "/fake/exiftool" }}

	path, ok := g.rawPreview(context.Background(), "/some/file.cr2")
	if !ok {
		t.Fatal("rawPreview ok=false, want true")
	}
	defer os.Remove(path)

	if len(rr.calls) != 1 || rr.calls[0] != "PreviewImage" {
		t.Errorf("tags tried = %v, want only [PreviewImage]", rr.calls)
	}
}

// TestRawPreviewNonJPEGRejected verifies a tag whose payload is not a JPEG is
// skipped (e.g. a stray non-image blob) and the chain continues.
func TestRawPreviewNonJPEGRejected(t *testing.T) {
	rr := &recordingRunner{byTag: map[string][]byte{
		"PreviewImage": []byte("not a jpeg"),
		"JpgFromRaw":   jpegBytes(t, 16, 16),
	}}
	g := qlGenerator{runExif: rr.run, exiftoolPath: func() string { return "/fake/exiftool" }}

	path, ok := g.rawPreview(context.Background(), "/some/file.nef")
	if !ok {
		t.Fatal("rawPreview ok=false, want true (JpgFromRaw is valid)")
	}
	defer os.Remove(path)
	if len(rr.calls) != 2 || rr.calls[1] != "JpgFromRaw" {
		t.Errorf("tags tried = %v, want [PreviewImage JpgFromRaw]", rr.calls)
	}
}

// TestRawPreviewNoExiftool verifies that when no exiftool binary is resolvable the
// extraction is disabled (ok=false) and the runner is never invoked, so the caller
// falls back to qlmanage.
func TestRawPreviewNoExiftool(t *testing.T) {
	rr := &recordingRunner{}
	g := qlGenerator{runExif: rr.run, exiftoolPath: func() string { return "" }}

	if _, ok := g.rawPreview(context.Background(), "/some/file.arw"); ok {
		t.Fatal("rawPreview ok=true, want false when exiftool absent")
	}
	if len(rr.calls) != 0 {
		t.Errorf("runner called %d times with no exiftool, want 0", len(rr.calls))
	}
}

// TestRawPreviewNoEmbeddedPreview verifies that a RAW with no embedded JPEG (every
// tag empty) yields ok=false after trying the whole chain.
func TestRawPreviewNoEmbeddedPreview(t *testing.T) {
	rr := &recordingRunner{byTag: map[string][]byte{}} // all tags return nil
	g := qlGenerator{runExif: rr.run, exiftoolPath: func() string { return "/fake/exiftool" }}

	if _, ok := g.rawPreview(context.Background(), "/some/file.orf"); ok {
		t.Fatal("rawPreview ok=true, want false when no embedded preview")
	}
	if len(rr.calls) != len(rawPreviewTags) {
		t.Errorf("tags tried = %d, want %d (whole chain)", len(rr.calls), len(rawPreviewTags))
	}
}

// TestGenerateRawFallsBackToQuickLook verifies that when preview extraction yields
// nothing for a RAW source, generate proceeds to the qlmanage branch: the whole
// exiftool tag chain is exhausted first, then the (stubbed) qlmanage render runs.
// The render is stubbed so the test never spawns qlmanage, which can block
// indefinitely on a missing/unrenderable source.
func TestGenerateRawFallsBackToQuickLook(t *testing.T) {
	rr := &recordingRunner{byTag: map[string][]byte{}} // extraction always empty
	var rendered atomic.Bool
	g := qlGenerator{
		runExif:      rr.run,
		exiftoolPath: func() string { return "/fake/exiftool" },
		qlRender: func(context.Context, string, string, int, int) error {
			rendered.Store(true)
			return nil
		},
	}

	if err := g.generate(context.Background(), "/some/file.raf", "/out.jpg", SizeGrid, defaultQuality); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(rr.calls) != len(rawPreviewTags) {
		t.Errorf("exiftool tags tried = %d, want %d before falling back", len(rr.calls), len(rawPreviewTags))
	}
	if !rendered.Load() {
		t.Error("qlmanage render was not reached after empty extraction")
	}
}

// TestGenerateNonRawSkipsExtraction verifies a non-RAW source never invokes the
// exiftool preview path (it goes straight to the qlmanage render).
func TestGenerateNonRawSkipsExtraction(t *testing.T) {
	rr := &recordingRunner{byTag: map[string][]byte{"PreviewImage": jpegBytes(t, 8, 8)}}
	var rendered atomic.Bool
	g := qlGenerator{
		runExif:      rr.run,
		exiftoolPath: func() string { return "/fake/exiftool" },
		qlRender: func(context.Context, string, string, int, int) error {
			rendered.Store(true)
			return nil
		},
	}

	if err := g.generate(context.Background(), "/some/photo.jpg", "/out.jpg", SizeGrid, defaultQuality); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(rr.calls) != 0 {
		t.Errorf("exiftool invoked %d times for a non-RAW source, want 0", len(rr.calls))
	}
	if !rendered.Load() {
		t.Error("qlmanage render was not reached for a non-RAW source")
	}
}

// TestRawPreviewIntegration extracts an embedded preview from a purpose-built file
// using the REAL exiftool. It builds a JPEG carrying an embedded ThumbnailImage,
// copies it to a .raf, and asserts rawPreview returns those exact preview bytes.
// Skips when exiftool is unavailable.
func TestRawPreviewIntegration(t *testing.T) {
	bin := requireExiftoolThumbs(t)

	dir := t.TempDir()
	host := filepath.Join(dir, "host.jpg")
	preview := filepath.Join(dir, "preview.jpg")
	if err := os.WriteFile(host, jpegBytes(t, 8, 8), 0o644); err != nil {
		t.Fatalf("write host: %v", err)
	}
	previewBytes := jpegBytes(t, 96, 72)
	if err := os.WriteFile(preview, previewBytes, 0o644); err != nil {
		t.Fatalf("write preview: %v", err)
	}
	// Embed the preview as a ThumbnailImage in the JPEG host, then present it under
	// a RAW extension. exiftool identifies by content, so a JPEG-bodied .raf reads
	// back its embedded preview exactly as a real RAW would.
	cmd := exec.Command(bin, "-overwrite_original", "-ThumbnailImage<="+preview, host)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("embed ThumbnailImage: %v\n%s", err, out)
	}
	raf := filepath.Join(dir, "fixture.raf")
	data, err := os.ReadFile(host)
	if err != nil {
		t.Fatalf("read host: %v", err)
	}
	if err := os.WriteFile(raf, data, 0o644); err != nil {
		t.Fatalf("write raf: %v", err)
	}

	g := newQLGenerator()
	path, ok := g.rawPreview(context.Background(), raf)
	if !ok {
		t.Fatal("rawPreview ok=false on a file with an embedded preview")
	}
	defer os.Remove(path)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read extracted preview: %v", err)
	}
	if !isJPEG(got) {
		t.Fatalf("extracted payload is not a JPEG (magic % x)", got[:min(2, len(got))])
	}
	if !bytes.Equal(got, previewBytes) {
		t.Errorf("extracted %d bytes, want the %d-byte embedded preview", len(got), len(previewBytes))
	}
}

// TestGenerateRawEmbeddedPreviewEndToEnd runs the full RAW path (extract preview →
// sips resize) with the real exiftool and sips, asserting a non-empty JPEG lands
// at dst. Skips when either tool is unavailable.
func TestGenerateRawEmbeddedPreviewEndToEnd(t *testing.T) {
	bin := requireExiftoolThumbs(t)
	if _, err := exec.LookPath("sips"); err != nil {
		t.Skip("sips not available")
	}

	dir := t.TempDir()
	host := filepath.Join(dir, "host.jpg")
	preview := filepath.Join(dir, "preview.jpg")
	if err := os.WriteFile(host, jpegBytes(t, 8, 8), 0o644); err != nil {
		t.Fatalf("write host: %v", err)
	}
	if err := os.WriteFile(preview, jpegBytes(t, 256, 192), 0o644); err != nil {
		t.Fatalf("write preview: %v", err)
	}
	cmd := exec.Command(bin, "-overwrite_original", "-ThumbnailImage<="+preview, host)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("embed ThumbnailImage: %v\n%s", err, out)
	}
	raf := filepath.Join(dir, "fixture.raf")
	data, _ := os.ReadFile(host)
	if err := os.WriteFile(raf, data, 0o644); err != nil {
		t.Fatalf("write raf: %v", err)
	}

	dst := filepath.Join(dir, "thumb.jpg")
	g := newQLGenerator()
	if err := g.generate(context.Background(), raf, dst, SizeGrid, defaultQuality); err != nil {
		t.Fatalf("generate RAW thumbnail: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil || info.Size() == 0 {
		t.Fatalf("expected a non-empty JPEG at %s (stat err=%v)", dst, err)
	}
	out, _ := os.ReadFile(dst)
	if !isJPEG(out) {
		t.Errorf("output is not a JPEG (magic % x)", out[:min(2, len(out))])
	}
}

// requireExiftoolThumbs skips the test unless an exiftool binary is resolvable.
func requireExiftoolThumbs(t *testing.T) string {
	t.Helper()
	bin := lookExiftool()
	if bin == "" {
		t.Skip("exiftool not installed; skipping integration test")
	}
	return bin
}
