package metadata

import (
	"context"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// requireExiftool skips the test unless an exiftool binary is resolvable.
func requireExiftool(t *testing.T) string {
	t.Helper()
	path := lookExiftool()
	if path == "" {
		t.Skip("exiftool not installed; skipping integration test")
	}
	return path
}

// writeTestJPEG creates a minimal valid 1x1 JPEG at path.
func writeTestJPEG(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create jpeg: %v", err)
	}
	defer f.Close()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	if err := jpeg.Encode(f, img, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
}

// TestExifToolRoundTrip writes EXIF into a tiny JPEG with exiftool itself, then
// extracts it back through the ExifTool implementation and asserts the values
// survive the round trip.
func TestExifToolRoundTrip(t *testing.T) {
	bin := requireExiftool(t)

	dir := t.TempDir()
	jpg := filepath.Join(dir, "roundtrip.jpg")
	writeTestJPEG(t, jpg)

	cmd := exec.Command(bin,
		"-overwrite_original",
		"-DateTimeOriginal=2021:01:02 03:04:05",
		"-Make=TestMake",
		"-Model=TestModel",
		"-ISO=200",
		"-FNumber=4.0",
		"-ExposureTime=1/250",
		jpg,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("writing exif with exiftool: %v\n%s", err, out)
	}

	et := newExifTool(bin, nil)
	defer et.Close()

	ctx := context.Background()
	m, err := et.Extract(ctx, jpg)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	if m.CameraMake != "TestMake" || m.CameraModel != "TestModel" {
		t.Errorf("make/model = %q/%q, want TestMake/TestModel", m.CameraMake, m.CameraModel)
	}
	if m.ISO != 200 {
		t.Errorf("ISO = %d, want 200", m.ISO)
	}
	if m.Aperture != "f/4" {
		t.Errorf("aperture = %q, want f/4", m.Aperture)
	}
	if m.ShutterSpeed != "1/250" {
		t.Errorf("shutter = %q, want 1/250", m.ShutterSpeed)
	}
	if m.CaptureDate == nil {
		t.Fatal("capture date is nil")
	}
	want := time.Date(2021, 1, 2, 3, 4, 5, 0, time.Local)
	if !m.CaptureDate.Equal(want) {
		t.Errorf("capture date = %v, want %v", m.CaptureDate, want)
	}
}

// TestExifToolBatch verifies batched extraction keys results by SourceFile.
func TestExifToolBatch(t *testing.T) {
	bin := requireExiftool(t)

	dir := t.TempDir()
	var paths []string
	for _, name := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		p := filepath.Join(dir, name)
		writeTestJPEG(t, p)
		cmd := exec.Command(bin, "-overwrite_original", "-Model=Batch-"+name, p)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("writing exif: %v\n%s", err, out)
		}
		paths = append(paths, p)
	}

	et := newExifTool(bin, nil)
	defer et.Close()

	got, err := et.ExtractBatch(context.Background(), paths)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(got) != len(paths) {
		t.Fatalf("got %d results, want %d", len(got), len(paths))
	}
	for _, p := range paths {
		m, ok := got[p]
		if !ok {
			t.Errorf("missing result for %s", p)
			continue
		}
		want := "Batch-" + filepath.Base(p)
		if m.CameraModel != want {
			t.Errorf("model for %s = %q, want %q", p, m.CameraModel, want)
		}
	}
}

// TestExifToolRestartAfterDeath verifies that when the persistent process is no
// longer running, the next request transparently starts a fresh one. This
// exercises the restart path used after a process failure.
func TestExifToolRestartAfterDeath(t *testing.T) {
	bin := requireExiftool(t)

	dir := t.TempDir()
	jpg := filepath.Join(dir, "restart.jpg")
	writeTestJPEG(t, jpg)
	cmd := exec.Command(bin, "-overwrite_original", "-Model=Restart", jpg)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("writing exif: %v\n%s", err, out)
	}

	et := newExifTool(bin, nil)
	defer et.Close()

	ctx := context.Background()
	if _, err := et.Extract(ctx, jpg); err != nil {
		t.Fatalf("first extract: %v", err)
	}

	// Tear the process down out from under the extractor, then confirm the next
	// call restarts it and succeeds.
	et.mu.Lock()
	et.stop()
	if et.cmd != nil {
		et.mu.Unlock()
		t.Fatal("stop() did not clear cmd")
	}
	et.mu.Unlock()

	m, err := et.Extract(ctx, jpg)
	if err != nil {
		t.Fatalf("extract after restart: %v", err)
	}
	if m.CameraModel != "Restart" {
		t.Errorf("model = %q, want Restart", m.CameraModel)
	}
}

// TestExifToolContextCancelled verifies a cancelled context aborts a request
// before the subprocess is even contacted (no exiftool binary required).
func TestExifToolContextCancelled(t *testing.T) {
	et := newExifTool("/nonexistent/exiftool", nil)
	defer et.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := et.Extract(ctx, "/whatever.jpg"); err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

// TestFallbackExtractor verifies the degraded extractor returns zero-value
// metadata without error and reports unavailability.
func TestFallbackExtractor(t *testing.T) {
	f := newFallback(nil)
	if f.Available() {
		t.Error("fallback should report Available() == false")
	}
	m, err := f.Extract(context.Background(), "/some/file.jpg")
	if err != nil {
		t.Fatalf("fallback extract: %v", err)
	}
	if m.SourceFile != "/some/file.jpg" {
		t.Errorf("source file = %q", m.SourceFile)
	}
	if m.CaptureDate != nil {
		t.Errorf("capture date = %v, want nil", m.CaptureDate)
	}

	batch, err := f.ExtractBatch(context.Background(), []string{"/a", "/b"})
	if err != nil {
		t.Fatalf("fallback batch: %v", err)
	}
	if len(batch) != 2 {
		t.Errorf("batch len = %d, want 2", len(batch))
	}
	if err := f.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
}

// TestNewExtractorSelection asserts the factory returns a usable extractor and
// that its availability matches whether exiftool is present.
func TestNewExtractorSelection(t *testing.T) {
	e := NewExtractor(nil)
	defer e.Close()

	wantAvailable := lookExiftool() != ""
	if e.Available() != wantAvailable {
		t.Errorf("Available() = %v, want %v", e.Available(), wantAvailable)
	}
}
