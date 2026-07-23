package rclone

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestGphotosPathMapping tables the album/ virtual-root mapping.
func TestGphotosPathMapping(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		want       string
	}{
		{"default prefixes album/", "PAIM-Backup", "album/PAIM-Backup"},
		{"nested default prefixes album/", "Photos/Archive", "album/Photos/Archive"},
		{"explicit album/ respected", "album/MyAlbum", "album/MyAlbum"},
		{"bare album respected", "album", "album"},
		{"explicit upload/ respected", "upload/staging", "upload/staging"},
		{"bare upload respected", "upload", "upload"},
		{"empty falls back to album", "", "album"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := gphotosPath(c.configured); got != c.want {
				t.Fatalf("gphotosPath(%q) = %q, want %q", c.configured, got, c.want)
			}
		})
	}
}

// TestRemotePathForGphotosVsFile asserts a Google Photos remote maps under album/
// (dir becomes the nested album name, basename the item) while a non-gphotos remote
// on the same plugin is untouched.
func TestRemotePathForGphotosVsFile(t *testing.T) {
	p := &Plugin{
		path:    "PAIM-Backup",
		gphotos: map[string]bool{"gphotos:": true},
	}
	rel := "2019/2019-06-12 Yosemite/DSCF0001.JPG"

	gotG := p.remotePathFor("gphotos:", rel)
	wantG := "gphotos:album/PAIM-Backup/2019/2019-06-12 Yosemite/DSCF0001.JPG"
	if gotG != wantG {
		t.Fatalf("gphotos remotePathFor = %q, want %q", gotG, wantG)
	}

	// A drive remote (not in the gphotos set) keeps the plain folder path.
	gotD := p.remotePathFor("gdrive:", rel)
	wantD := "gdrive:PAIM-Backup/2019/2019-06-12 Yosemite/DSCF0001.JPG"
	if gotD != wantD {
		t.Fatalf("non-gphotos remotePathFor = %q, want %q", gotD, wantD)
	}
}

// TestRemotePathForRespectsExplicitAlbumRoot asserts a power-user path already
// rooted at album/ is not double-prefixed for a gphotos remote.
func TestRemotePathForRespectsExplicitAlbumRoot(t *testing.T) {
	p := &Plugin{path: "album/Trips", gphotos: map[string]bool{"gp:": true}}
	got := p.remotePathFor("gp:", "2020/x.jpg")
	want := "gp:album/Trips/2020/x.jpg"
	if got != want {
		t.Fatalf("remotePathFor = %q, want %q", got, want)
	}
}

// gphotosResponder answers listremotes + `config show` (type=googlephotos) and
// defers copyto/other calls to next.
func gphotosResponder(remotes []string, next func(ctx context.Context, args []string, onLine func(string)) ([]byte, []byte, error)) func(context.Context, []string, func(string)) ([]byte, []byte, error) {
	return func(ctx context.Context, args []string, onLine func(string)) ([]byte, []byte, error) {
		if len(args) > 0 && args[0] == "listremotes" {
			out := ""
			for _, r := range remotes {
				out += r + "\n"
			}
			return []byte(out), nil, nil
		}
		if len(args) >= 2 && args[0] == "config" && args[1] == "show" {
			return []byte("[" + args[2] + "]\ntype = googlephotos\n"), nil, nil
		}
		if next != nil {
			return next(ctx, args, onLine)
		}
		return nil, nil, nil
	}
}

// TestUploadUsesMappedGphotosPath drives a full Initialize+Upload against a fake
// runner and asserts the copyto destination is the album-mapped path.
func TestUploadUsesMappedGphotosPath(t *testing.T) {
	runner := &fakeRunner{responder: gphotosResponder([]string{"gphotos:"}, nil)}
	p := newTestPlugin(runner)

	// gphotos is hashless -> only valid as a mirror; Initialize accepts a single
	// remote regardless. mirror=true keeps it consistent with real configs.
	if err := p.Initialize(context.Background(), `{"remote":"gphotos:","path":"PAIM-Backup","mirror":true}`); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if !p.gphotos["gphotos:"] {
		t.Fatalf("expected gphotos remote to be flagged")
	}

	dir := t.TempDir()
	local := filepath.Join(dir, "DSCF0001.JPG")
	if err := os.WriteFile(local, []byte("bytes"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}

	if err := p.Upload(context.Background(), local, "2019/2019-06-12 Yosemite/DSCF0001.JPG", nil); err != nil {
		t.Fatalf("upload: %v", err)
	}

	copyto := runner.callWith("copyto")
	if copyto == nil {
		t.Fatal("no copyto call recorded")
	}
	dst := copyto[len(copyto)-1]
	want := "gphotos:album/PAIM-Backup/2019/2019-06-12 Yosemite/DSCF0001.JPG"
	if dst != want {
		t.Fatalf("copyto dst = %q, want %q", dst, want)
	}
}
