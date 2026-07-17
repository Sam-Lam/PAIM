package metadata

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// loadFixture parses a golden exiftool JSON fixture and returns its single
// record, failing the test on any error.
func loadFixture(t *testing.T, name string) *AssetMetadata {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	metas, err := parseExifJSON(data)
	if err != nil {
		t.Fatalf("parsing fixture %s: %v", name, err)
	}
	if len(metas) != 1 {
		t.Fatalf("fixture %s: expected 1 record, got %d", name, len(metas))
	}
	return metas[0]
}

func TestParseFujiRAF(t *testing.T) {
	m := loadFixture(t, "fuji_raf.json")

	if m.CameraMake != "FUJIFILM" || m.CameraModel != "X-T4" {
		t.Errorf("make/model = %q/%q", m.CameraMake, m.CameraModel)
	}
	if m.Lens != "XF16-55mmF2.8 R LM WR" {
		t.Errorf("lens = %q", m.Lens)
	}
	if m.ISO != 160 {
		t.Errorf("ISO = %d, want 160", m.ISO)
	}
	if m.ShutterSpeed != "1/125" {
		t.Errorf("shutter = %q, want 1/125", m.ShutterSpeed)
	}
	if m.Aperture != "f/2.8" {
		t.Errorf("aperture = %q, want f/2.8", m.Aperture)
	}
	if m.Width != 6240 || m.Height != 4160 {
		t.Errorf("dimensions = %dx%d, want 6240x4160", m.Width, m.Height)
	}
	if m.ColorSpace != "sRGB" {
		t.Errorf("colorspace = %q, want sRGB", m.ColorSpace)
	}
	// SubSecDateTimeOriginal (naive, local) takes priority over DateTimeOriginal.
	if m.CaptureDate == nil {
		t.Fatal("capture date is nil")
	}
	want := time.Date(2026, 5, 1, 14, 30, 0, 500_000_000, time.Local)
	if !m.CaptureDate.Equal(want) {
		t.Errorf("capture date = %v, want %v", m.CaptureDate, want)
	}
}

func TestParseIPhoneHEIC(t *testing.T) {
	m := loadFixture(t, "iphone_heic.json")

	// Orientation 6 means a 90-degree rotation: stored 4032x3024 -> displayed
	// 3024x4032.
	if m.Orientation != 6 {
		t.Errorf("orientation = %d, want 6", m.Orientation)
	}
	if m.Width != 3024 || m.Height != 4032 {
		t.Errorf("dimensions = %dx%d, want 3024x4032 (orientation-swapped)", m.Width, m.Height)
	}
	if m.GPSLatitude == nil || *m.GPSLatitude != 37.7749 {
		t.Errorf("latitude = %v, want 37.7749", m.GPSLatitude)
	}
	// Longitude reference W must produce a negative value.
	if m.GPSLongitude == nil || *m.GPSLongitude != -122.4194 {
		t.Errorf("longitude = %v, want -122.4194", m.GPSLongitude)
	}
	if m.ContentIdentifier != "E1B2C3D4-1234-5678-9ABC-DEF012345678" {
		t.Errorf("content id = %q", m.ContentIdentifier)
	}
	if m.ColorSpace != "Uncalibrated" {
		t.Errorf("colorspace = %q, want Uncalibrated", m.ColorSpace)
	}
	if m.ShutterSpeed != "1/60" {
		t.Errorf("shutter = %q, want 1/60", m.ShutterSpeed)
	}
	if m.Aperture != "f/1.78" {
		t.Errorf("aperture = %q, want f/1.78", m.Aperture)
	}

	// Timezone-aware capture date with sub-second precision.
	if m.CaptureDate == nil {
		t.Fatal("capture date is nil")
	}
	zone := time.FixedZone("", -7*3600)
	want := time.Date(2026, 7, 4, 9, 15, 30, 123_000_000, zone)
	if !m.CaptureDate.Equal(want) {
		t.Errorf("capture date = %v, want %v", m.CaptureDate, want)
	}
	if _, offset := m.CaptureDate.Zone(); offset != -7*3600 {
		t.Errorf("tz offset = %d, want -25200", offset)
	}
}

func TestParseVideoMOV(t *testing.T) {
	m := loadFixture(t, "video_mov.json")

	if m.DurationSeconds != 12.5 {
		t.Errorf("duration = %v, want 12.5", m.DurationSeconds)
	}
	if m.FrameRate != 29.97 {
		t.Errorf("frame rate = %v, want 29.97", m.FrameRate)
	}
	if m.Codec != "hvc1" {
		t.Errorf("codec = %q, want hvc1", m.Codec)
	}
	if m.Width != 1920 || m.Height != 1080 {
		t.Errorf("dimensions = %dx%d, want 1920x1080", m.Width, m.Height)
	}
	// CreateDate is the only date present (naive, local).
	if m.CaptureDate == nil {
		t.Fatal("capture date is nil")
	}
	want := time.Date(2026, 7, 4, 9, 20, 0, 0, time.Local)
	if !m.CaptureDate.Equal(want) {
		t.Errorf("capture date = %v, want %v", m.CaptureDate, want)
	}
}

func TestParseMissingFields(t *testing.T) {
	m := loadFixture(t, "missing_fields.json")

	if m.CaptureDate != nil {
		t.Errorf("capture date = %v, want nil", m.CaptureDate)
	}
	if m.GPSLatitude != nil || m.GPSLongitude != nil {
		t.Errorf("gps = %v/%v, want nil", m.GPSLatitude, m.GPSLongitude)
	}
	if m.CameraMake != "" || m.CameraModel != "" || m.Lens != "" {
		t.Errorf("expected empty camera fields, got %q/%q/%q", m.CameraMake, m.CameraModel, m.Lens)
	}
	if m.ISO != 0 || m.ShutterSpeed != "" || m.Aperture != "" {
		t.Errorf("expected zero exposure fields, got %d/%q/%q", m.ISO, m.ShutterSpeed, m.Aperture)
	}
	if m.Width != 0 || m.Height != 0 {
		t.Errorf("expected zero dimensions, got %dx%d", m.Width, m.Height)
	}
	if m.ContentIdentifier != "" {
		t.Errorf("content id = %q, want empty", m.ContentIdentifier)
	}
}

func TestParseSubSecTimezonePriority(t *testing.T) {
	m := loadFixture(t, "subsec_tz.json")

	if m.CaptureDate == nil {
		t.Fatal("capture date is nil")
	}
	zone := time.FixedZone("", -7*3600)
	want := time.Date(2026, 7, 17, 10, 4, 5, 123_000_000, zone)
	if !m.CaptureDate.Equal(want) {
		t.Errorf("capture date = %v, want %v (SubSec must win over DateTimeOriginal/CreateDate)",
			m.CaptureDate, want)
	}
	if _, offset := m.CaptureDate.Zone(); offset != -7*3600 {
		t.Errorf("tz offset = %d, want -25200", offset)
	}
}

func TestParseNaiveDate(t *testing.T) {
	m := loadFixture(t, "naive_date.json")

	if m.CaptureDate == nil {
		t.Fatal("capture date is nil")
	}
	want := time.Date(2026, 7, 17, 10, 4, 5, 0, time.Local)
	if !m.CaptureDate.Equal(want) {
		t.Errorf("capture date = %v, want %v", m.CaptureDate, want)
	}
	// A naive value must be interpreted in the local zone.
	if m.CaptureDate.Location() != time.Local {
		t.Errorf("location = %v, want Local", m.CaptureDate.Location())
	}
}

func TestParseExifTimeVariants(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"empty", "", false},
		{"allzero", "0000:00:00 00:00:00", false},
		{"naive", "2026:07:17 10:04:05", true},
		{"naive_subsec", "2026:07:17 10:04:05.5", true},
		{"tz", "2026:07:17 10:04:05-07:00", true},
		{"tz_subsec", "2026:07:17 10:04:05.123-07:00", true},
		{"utc_z", "2026:07:17 10:04:05Z", true},
		{"garbage", "not a date", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := parseExifTime(tc.in)
			if ok != tc.ok {
				t.Errorf("parseExifTime(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			}
		})
	}
}

func TestFormatShutterSpeed(t *testing.T) {
	cases := []struct {
		exposure float64
		want     string
	}{
		{0.002, "1/500"},
		{0.008, "1/125"},
		{0.5, "1/2"},
		{1, "1"},
		{2.5, "2.5"},
		{0, ""},
	}
	for _, tc := range cases {
		r := record{"ExposureTime": []byte(strconv.FormatFloat(tc.exposure, 'g', -1, 64))}
		if got := formatShutterSpeed(r); got != tc.want {
			t.Errorf("formatShutterSpeed(%v) = %q, want %q", tc.exposure, got, tc.want)
		}
	}
}

func TestParseBatchMultipleRecords(t *testing.T) {
	data := []byte(`[
      {"SourceFile":"/a.jpg","Make":"A"},
      {"SourceFile":"/b.jpg","Make":"B"}
    ]`)
	metas, err := parseExifJSON(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("got %d records, want 2", len(metas))
	}
	if metas[0].SourceFile != "/a.jpg" || metas[1].SourceFile != "/b.jpg" {
		t.Errorf("source files = %q,%q", metas[0].SourceFile, metas[1].SourceFile)
	}
}

func TestParseInvalidJSON(t *testing.T) {
	if _, err := parseExifJSON([]byte("{not json")); err == nil {
		t.Error("expected error for malformed json")
	}
}
