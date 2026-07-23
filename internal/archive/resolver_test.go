package archive

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Sam-Lam/PAIM/internal/mediatype"
)

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", dir, err)
	}
}

func TestDestinationResolverSticky(t *testing.T) {
	d := date(t, "2026-07-17")

	t.Run("zero_matches_bare", func(t *testing.T) {
		root := t.TempDir()
		r := NewDestinationResolver(New(root))
		got := r.DestinationFor(d, "", "IMG_1.jpg", mediatype.Photo)
		want := filepath.Join("2026", "2026-07-17", "IMG_1.jpg")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("one_labeled_match_joins", func(t *testing.T) {
		root := t.TempDir()
		mkdir(t, filepath.Join(root, "2026", "2026-07-17 Yosemite"))
		r := NewDestinationResolver(New(root))
		got := r.DestinationFor(d, "", "IMG_1.jpg", mediatype.Photo)
		want := filepath.Join("2026", "2026-07-17 Yosemite", "IMG_1.jpg")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("one_bare_match_joins_bare", func(t *testing.T) {
		root := t.TempDir()
		mkdir(t, filepath.Join(root, "2026", "2026-07-17"))
		r := NewDestinationResolver(New(root))
		got := r.DestinationFor(d, "", "IMG_1.jpg", mediatype.Photo)
		want := filepath.Join("2026", "2026-07-17", "IMG_1.jpg")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("two_matches_bare", func(t *testing.T) {
		root := t.TempDir()
		mkdir(t, filepath.Join(root, "2026", "2026-07-17 Yosemite"))
		mkdir(t, filepath.Join(root, "2026", "2026-07-17 Beach"))
		r := NewDestinationResolver(New(root))
		got := r.DestinationFor(d, "", "IMG_1.jpg", mediatype.Photo)
		want := filepath.Join("2026", "2026-07-17", "IMG_1.jpg")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("bare_plus_labeled_is_two_matches_bare", func(t *testing.T) {
		root := t.TempDir()
		mkdir(t, filepath.Join(root, "2026", "2026-07-17"))
		mkdir(t, filepath.Join(root, "2026", "2026-07-17 Yosemite"))
		r := NewDestinationResolver(New(root))
		got := r.DestinationFor(d, "", "IMG_1.jpg", mediatype.Photo)
		want := filepath.Join("2026", "2026-07-17", "IMG_1.jpg")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("boundary_prefix_not_a_match", func(t *testing.T) {
		root := t.TempDir()
		// A different date whose bare form is a string-prefix of ours must NOT match.
		mkdir(t, filepath.Join(root, "2026", "2026-07-170 Weird"))
		r := NewDestinationResolver(New(root))
		got := r.DestinationFor(d, "", "IMG_1.jpg", mediatype.Photo)
		want := filepath.Join("2026", "2026-07-17", "IMG_1.jpg")
		if got != want {
			t.Errorf("got %q, want %q (segment boundary must be respected)", got, want)
		}
	})

	t.Run("explicit_event_exact_never_sticky", func(t *testing.T) {
		root := t.TempDir()
		mkdir(t, filepath.Join(root, "2026", "2026-07-17 Yosemite"))
		r := NewDestinationResolver(New(root))
		got := r.DestinationFor(d, "Beach", "IMG_1.jpg", mediatype.Photo)
		want := filepath.Join("2026", "2026-07-17 Beach", "IMG_1.jpg")
		if got != want {
			t.Errorf("got %q, want %q (explicit event must be exact)", got, want)
		}
	})

	t.Run("raw_routed_to_subfolder_under_sticky", func(t *testing.T) {
		root := t.TempDir()
		mkdir(t, filepath.Join(root, "2026", "2026-07-17 Yosemite"))
		r := NewDestinationResolver(New(root))
		got := r.DestinationFor(d, "", "IMG_1.cr3", mediatype.RawPhoto)
		want := filepath.Join("2026", "2026-07-17 Yosemite", "RAW", "IMG_1.cr3")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

// TestDestinationResolverCacheDeterministic proves the resolution is cached and
// stable across a run even if the filesystem changes after the first lookup: the
// first (labeled) resolution is reused for every later file of the same date.
func TestDestinationResolverCacheDeterministic(t *testing.T) {
	d := date(t, "2026-07-17")
	root := t.TempDir()
	mkdir(t, filepath.Join(root, "2026", "2026-07-17 Yosemite"))
	r := NewDestinationResolver(New(root))

	first := r.DestinationFor(d, "", "IMG_1.jpg", mediatype.Photo)

	// Introduce a second labeled folder AFTER the first resolution. A fresh
	// resolver would now see 2 matches and pick bare; the cached one must not.
	mkdir(t, filepath.Join(root, "2026", "2026-07-17 Beach"))

	second := r.DestinationFor(d, "", "IMG_2.jpg", mediatype.Photo)
	if filepath.Dir(first) != filepath.Dir(second) {
		t.Errorf("cache not deterministic: %q vs %q", filepath.Dir(first), filepath.Dir(second))
	}
	if filepath.Dir(second) != filepath.Join("2026", "2026-07-17 Yosemite") {
		t.Errorf("second resolved to %q, want the cached Yosemite folder", filepath.Dir(second))
	}

	// A brand-new resolver reflects the current disk (now ambiguous → bare).
	fresh := NewDestinationResolver(New(root)).DestinationFor(d, "", "IMG_3.jpg", mediatype.Photo)
	if filepath.Dir(fresh) != filepath.Join("2026", "2026-07-17") {
		t.Errorf("fresh resolver = %q, want bare date folder (2 matches)", filepath.Dir(fresh))
	}
}
