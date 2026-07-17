package archive

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/autolinepro/paim/internal/mediatype"
)

func date(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse date: %v", err)
	}
	return ts
}

func TestDestinationFor(t *testing.T) {
	l := New("/master")
	d := date(t, "2026-07-17")

	cases := []struct {
		name  string
		event string
		file  string
		kind  mediatype.Kind
		want  string
	}{
		{
			name:  "photo_with_event",
			event: "Beach Trip",
			file:  "IMG_0001.jpg",
			kind:  mediatype.Photo,
			want:  filepath.Join("2026", "2026-07-17 Beach Trip", "IMG_0001.jpg"),
		},
		{
			name:  "video_with_event",
			event: "Beach Trip",
			file:  "MVI_0002.mov",
			kind:  mediatype.Video,
			want:  filepath.Join("2026", "2026-07-17 Beach Trip", "MVI_0002.mov"),
		},
		{
			name:  "raw_routed_to_subfolder",
			event: "Beach Trip",
			file:  "IMG_0003.cr3",
			kind:  mediatype.RawPhoto,
			want:  filepath.Join("2026", "2026-07-17 Beach Trip", "RAW", "IMG_0003.cr3"),
		},
		{
			name:  "empty_event_date_only",
			event: "",
			file:  "IMG_0004.jpg",
			kind:  mediatype.Photo,
			want:  filepath.Join("2026", "2026-07-17", "IMG_0004.jpg"),
		},
		{
			name:  "raw_empty_event",
			event: "",
			file:  "IMG_0005.nef",
			kind:  mediatype.RawPhoto,
			want:  filepath.Join("2026", "2026-07-17", "RAW", "IMG_0005.nef"),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := l.DestinationFor(d, c.event, c.file, c.kind)
			if got != c.want {
				t.Errorf("DestinationFor = %q, want %q", got, c.want)
			}
		})
	}
}

func TestEventSanitization(t *testing.T) {
	l := New("/master")
	d := date(t, "2026-01-05")

	cases := []struct {
		event string
		want  string // resulting day folder
	}{
		{"Trip/../etc", "2026-01-05 Trip..etc"}, // '/' stripped, no separators remain
		{"a/b\\c:d", "2026-01-05 abcd"},         // all separators stripped
		{"  spaced  ", "2026-01-05 spaced"},     // trimmed
		{"...", "2026-01-05"},                   // dots-only trims to empty -> date only
		{"tab\there", "2026-01-05 tabhere"},     // control char stripped
		{"", "2026-01-05"},                      // empty -> date only
	}
	for _, c := range cases {
		got := l.DestinationFor(d, c.event, "f.jpg", mediatype.Photo)
		want := filepath.Join("2026", c.want, "f.jpg")
		if got != want {
			t.Errorf("event %q -> %q, want %q", c.event, got, want)
		}
	}
}

// TestEventSanitizationNoSeparators guards the key safety property: a sanitized
// event never introduces extra path components.
func TestEventSanitizationNoSeparators(t *testing.T) {
	l := New("/master")
	d := date(t, "2026-01-05")
	got := l.DestinationFor(d, "a/b/c", "f.jpg", mediatype.Photo)
	// Expect exactly: year / day / file = 3 components (2 separators).
	count := 1
	for _, r := range got {
		if r == filepath.Separator {
			count++
		}
	}
	if count != 3 {
		t.Errorf("expected 3 path components, got %d in %q", count, got)
	}
}

func TestResolveCollision(t *testing.T) {
	dir := t.TempDir()

	// No collision: returned unchanged.
	got, err := ResolveCollision(dir, "photo.jpg")
	if err != nil {
		t.Fatalf("ResolveCollision: %v", err)
	}
	if got != "photo.jpg" {
		t.Errorf("got %q, want photo.jpg", got)
	}

	// Create photo.jpg -> expect "photo (2).jpg".
	touch(t, dir, "photo.jpg")
	got, err = ResolveCollision(dir, "photo.jpg")
	if err != nil {
		t.Fatalf("ResolveCollision: %v", err)
	}
	if got != "photo (2).jpg" {
		t.Errorf("got %q, want photo (2).jpg", got)
	}

	// Create "photo (2).jpg" too -> expect "photo (3).jpg".
	touch(t, dir, "photo (2).jpg")
	got, err = ResolveCollision(dir, "photo.jpg")
	if err != nil {
		t.Fatalf("ResolveCollision: %v", err)
	}
	if got != "photo (3).jpg" {
		t.Errorf("got %q, want photo (3).jpg", got)
	}
}

// TestResolveCollisionIgnoresPartials confirms .paim-partial-* temp files are
// not treated as occupying a final name.
func TestResolveCollisionIgnoresPartials(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, ".paim-partial-abcd")
	got, err := ResolveCollision(dir, "photo.jpg")
	if err != nil {
		t.Fatalf("ResolveCollision: %v", err)
	}
	if got != "photo.jpg" {
		t.Errorf("got %q, want photo.jpg (partials must be ignored)", got)
	}
}

func TestResolveCollisionNoExt(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "README")
	got, err := ResolveCollision(dir, "README")
	if err != nil {
		t.Fatalf("ResolveCollision: %v", err)
	}
	if got != "README (2)" {
		t.Errorf("got %q, want \"README (2)\"", got)
	}
}

func touch(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
		t.Fatalf("touch %s: %v", name, err)
	}
}
