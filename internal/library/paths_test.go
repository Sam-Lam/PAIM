package library

import (
	"path/filepath"
	"testing"
)

func TestResolvePath(t *testing.T) {
	root := filepath.Join("/", "Volumes", "Photos")
	cases := []struct {
		name, root, stored, want string
	}{
		{"empty stays empty", root, "", ""},
		{"relative joins root", root, "2024/2024-01-01/IMG.JPG", filepath.Join(root, "2024", "2024-01-01", "IMG.JPG")},
		{"absolute passes through", root, "/other/place/IMG.JPG", "/other/place/IMG.JPG"},
		{"relative with empty root is native", "", "2024/IMG.JPG", filepath.FromSlash("2024/IMG.JPG")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolvePath(c.root, c.stored); got != c.want {
				t.Fatalf("ResolvePath(%q,%q) = %q, want %q", c.root, c.stored, got, c.want)
			}
		})
	}
}

func TestRelativizePath(t *testing.T) {
	root := filepath.Join("/", "Volumes", "Photos")
	inside := filepath.Join(root, "2024", "2024-01-01", "IMG.JPG")
	outside := filepath.Join("/", "elsewhere", "IMG.JPG")

	if got := RelativizePath(root, inside); got != "2024/2024-01-01/IMG.JPG" {
		t.Fatalf("inside root: got %q", got)
	}
	if got := RelativizePath(root, outside); got != outside {
		t.Fatalf("outside root should stay absolute: got %q", got)
	}
	if got := RelativizePath(root, ""); got != "" {
		t.Fatalf("empty stays empty: got %q", got)
	}
	if got := RelativizePath("", inside); got != inside {
		t.Fatalf("empty root returns input: got %q", got)
	}
	// Round-trips: relativize then resolve yields the original absolute path.
	if got := ResolvePath(root, RelativizePath(root, inside)); got != inside {
		t.Fatalf("round trip inside: got %q want %q", got, inside)
	}
}
