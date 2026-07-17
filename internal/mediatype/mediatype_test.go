package mediatype

import (
	"testing"

	"github.com/autolinepro/paim/internal/domain"
)

func TestKind(t *testing.T) {
	cases := []struct {
		ext  string
		want Kind
	}{
		{"jpg", Photo},
		{"JPG", Photo},
		{".jpeg", Photo},
		{".PNG", Photo},
		{"tiff", Photo},
		{"tif", Photo},
		{"heic", Photo},
		{"avif", Photo},
		{"raf", RawPhoto},
		{"cr2", RawPhoto},
		{"CR3", RawPhoto},
		{".arw", RawPhoto},
		{"nef", RawPhoto},
		{"orf", RawPhoto},
		{"rw2", RawPhoto},
		{"dng", RawPhoto},
		{"mov", Video},
		{".MP4", Video},
		{"m4v", Video},
		{"avi", Video},
		{"mxf", Video},
		{"txt", Unknown},
		{"", Unknown},
		{".", Unknown},
		{"xmp", Unknown},
	}
	for _, c := range cases {
		if got := KindOf(c.ext); got != c.want {
			t.Errorf("KindOf(%q) = %v, want %v", c.ext, got, c.want)
		}
	}
}

func TestIsMedia(t *testing.T) {
	cases := map[string]bool{
		"jpg": true,
		"dng": true,
		"mov": true,
		"txt": false,
		"":    false,
	}
	for ext, want := range cases {
		if got := IsMedia(ext); got != want {
			t.Errorf("IsMedia(%q) = %v, want %v", ext, got, want)
		}
	}
}

func TestMediaTypeFor(t *testing.T) {
	cases := []struct {
		ext  string
		want domain.MediaType
	}{
		{"jpg", domain.MediaTypePhoto},
		{"cr3", domain.MediaTypeRawPhoto},
		{"mp4", domain.MediaTypeVideo},
		{"txt", domain.MediaType("")},
	}
	for _, c := range cases {
		if got := MediaTypeFor(c.ext); got != c.want {
			t.Errorf("MediaTypeFor(%q) = %q, want %q", c.ext, got, c.want)
		}
	}
}

func TestKindString(t *testing.T) {
	cases := map[Kind]string{
		Photo:    "photo",
		RawPhoto: "raw_photo",
		Video:    "video",
		Unknown:  "unknown",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", k, got, want)
		}
	}
}

func TestPairLivePhotos(t *testing.T) {
	cases := []struct {
		name  string
		files []Candidate
		want  []Pair // compared by Still.Path + Motion.Path
	}{
		{
			name: "heic_mov_no_identifiers",
			files: []Candidate{
				{Path: "/src/IMG_0001.HEIC", Ext: "heic"},
				{Path: "/src/IMG_0001.MOV", Ext: "mov"},
			},
			want: []Pair{{
				Still:  Candidate{Path: "/src/IMG_0001.HEIC", Ext: "heic"},
				Motion: Candidate{Path: "/src/IMG_0001.MOV", Ext: "mov"},
			}},
		},
		{
			name: "jpg_mov_matching_identifiers",
			files: []Candidate{
				{Path: "/src/IMG_0002.JPG", Ext: "jpg", ContentIdentifier: "ABC"},
				{Path: "/src/IMG_0002.MOV", Ext: "mov", ContentIdentifier: "ABC"},
			},
			want: []Pair{{
				Still:  Candidate{Path: "/src/IMG_0002.JPG", Ext: "jpg", ContentIdentifier: "ABC"},
				Motion: Candidate{Path: "/src/IMG_0002.MOV", Ext: "mov", ContentIdentifier: "ABC"},
			}},
		},
		{
			name: "identifier_mismatch_prevents_pairing",
			files: []Candidate{
				{Path: "/src/IMG_0003.HEIC", Ext: "heic", ContentIdentifier: "ABC"},
				{Path: "/src/IMG_0003.MOV", Ext: "mov", ContentIdentifier: "XYZ"},
			},
			want: nil,
		},
		{
			name: "one_identifier_empty_pairs_on_basename",
			files: []Candidate{
				{Path: "/src/IMG_0004.HEIC", Ext: "heic", ContentIdentifier: "ABC"},
				{Path: "/src/IMG_0004.MOV", Ext: "mov"},
			},
			want: []Pair{{
				Still:  Candidate{Path: "/src/IMG_0004.HEIC", Ext: "heic", ContentIdentifier: "ABC"},
				Motion: Candidate{Path: "/src/IMG_0004.MOV", Ext: "mov"},
			}},
		},
		{
			name: "different_directories_do_not_pair",
			files: []Candidate{
				{Path: "/a/IMG_0005.HEIC", Ext: "heic"},
				{Path: "/b/IMG_0005.MOV", Ext: "mov"},
			},
			want: nil,
		},
		{
			name: "mov_without_still_ignored",
			files: []Candidate{
				{Path: "/src/CLIP.MOV", Ext: "mov"},
			},
			want: nil,
		},
		{
			name: "still_without_mov_ignored",
			files: []Candidate{
				{Path: "/src/IMG_0006.HEIC", Ext: "heic"},
			},
			want: nil,
		},
		{
			name: "ambiguous_two_movs_not_paired",
			files: []Candidate{
				{Path: "/src/IMG_0007.HEIC", Ext: "heic"},
				{Path: "/src/IMG_0007.MOV", Ext: "mov"},
				{Path: "/src/IMG_0007.mov", Ext: "mov"}, // same basename+dir key
			},
			want: nil,
		},
		{
			name: "multiple_pairs_in_order",
			files: []Candidate{
				{Path: "/src/B.HEIC", Ext: "heic"},
				{Path: "/src/B.MOV", Ext: "mov"},
				{Path: "/src/A.HEIC", Ext: "heic"},
				{Path: "/src/A.MOV", Ext: "mov"},
			},
			want: []Pair{
				{Still: Candidate{Path: "/src/B.HEIC", Ext: "heic"}, Motion: Candidate{Path: "/src/B.MOV", Ext: "mov"}},
				{Still: Candidate{Path: "/src/A.HEIC", Ext: "heic"}, Motion: Candidate{Path: "/src/A.MOV", Ext: "mov"}},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PairLivePhotos(c.files)
			if len(got) != len(c.want) {
				t.Fatalf("got %d pairs, want %d: %+v", len(got), len(c.want), got)
			}
			for i := range got {
				if got[i].Still.Path != c.want[i].Still.Path || got[i].Motion.Path != c.want[i].Motion.Path {
					t.Errorf("pair %d = (%s, %s), want (%s, %s)", i,
						got[i].Still.Path, got[i].Motion.Path,
						c.want[i].Still.Path, c.want[i].Motion.Path)
				}
			}
		})
	}
}
