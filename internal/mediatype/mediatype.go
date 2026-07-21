// Package mediatype is the registry of file extensions PAIM understands and the
// rules for classifying them into media kinds. It also implements Live Photo
// pairing, matching a still (HEIC or JPG) with its companion MOV.
//
// Classification is case-insensitive and tolerant of a leading dot, so "JPG",
// ".jpg" and "jpg" are equivalent.
package mediatype

import (
	"path/filepath"
	"strings"

	"github.com/Sam-Lam/PAIM/internal/domain"
)

// Kind is the coarse classification of a media file by extension.
type Kind int

// Kind values. Unknown covers any extension not in the registry (non-media or
// unsupported).
const (
	Unknown Kind = iota
	Photo
	RawPhoto
	Video
)

// String returns a human-readable name for the Kind.
func (k Kind) String() string {
	switch k {
	case Photo:
		return "photo"
	case RawPhoto:
		return "raw_photo"
	case Video:
		return "video"
	default:
		return "unknown"
	}
}

// registry maps a normalized (lowercase, no dot) extension to its Kind. It is
// built once at package init from the spec's supported extension lists.
var registry = func() map[string]Kind {
	m := make(map[string]Kind)
	for _, ext := range []string{"jpg", "jpeg", "png", "tiff", "tif", "heic", "avif"} {
		m[ext] = Photo
	}
	for _, ext := range []string{"raf", "cr2", "cr3", "arw", "nef", "orf", "rw2", "dng"} {
		m[ext] = RawPhoto
	}
	for _, ext := range []string{"mov", "mp4", "m4v", "avi", "mxf"} {
		m[ext] = Video
	}
	return m
}()

// normalizeExt lowercases ext and strips a single leading dot so callers may
// pass "JPG", ".jpg", or "jpg" interchangeably.
func normalizeExt(ext string) string {
	return strings.ToLower(strings.TrimPrefix(ext, "."))
}

// KindOf returns the media Kind for the given extension. Unrecognized
// extensions return Unknown. (It is named KindOf rather than Kind because Kind
// is already the name of the returned type.)
func KindOf(ext string) Kind {
	return registry[normalizeExt(ext)]
}

// IsMedia reports whether ext is a supported media extension (photo, RAW, or
// video).
func IsMedia(ext string) bool {
	return KindOf(ext) != Unknown
}

// MediaTypeFor maps an extension to the persistent domain.MediaType. A
// non-media extension returns an empty MediaType so callers can detect "not
// media"; use IsMedia to guard. Live Photo pairing is resolved separately (see
// PairLivePhotos) and is not a property of a single extension.
func MediaTypeFor(ext string) domain.MediaType {
	switch KindOf(ext) {
	case Photo:
		return domain.MediaTypePhoto
	case RawPhoto:
		return domain.MediaTypeRawPhoto
	case Video:
		return domain.MediaTypeVideo
	default:
		return domain.MediaType("")
	}
}

// Candidate is a file considered for Live Photo pairing. ContentIdentifier is
// the exiftool ContentIdentifier value and may be empty when unavailable.
type Candidate struct {
	Path              string
	Ext               string
	ContentIdentifier string
}

// Pair links a still image with its companion motion (MOV) file, forming one
// logical Live Photo asset.
type Pair struct {
	Still  Candidate
	Motion Candidate
}

// PairLivePhotos groups candidates into Live Photo pairs. A pair is a still
// (HEIC or JPG) and a MOV that share the same basename in the same directory.
// When both files carry a ContentIdentifier they must match; a mismatch
// prevents pairing. If either identifier is empty, the shared basename in the
// same directory is sufficient.
//
// Only unambiguous one-to-one matches are paired: a still is paired with a
// motion file only when exactly one eligible motion file shares its
// directory+basename key. Candidates that do not pair are simply omitted from
// the result. Input order of the stills determines output order.
func PairLivePhotos(files []Candidate) []Pair {
	type key struct{ dir, base string }

	stills := make(map[key][]Candidate)
	motions := make(map[key][]Candidate)

	for _, f := range files {
		ext := normalizeExt(f.Ext)
		k := key{
			dir:  filepath.Dir(f.Path),
			base: basenameNoExt(f.Path),
		}
		switch {
		case ext == "heic" || ext == "jpg" || ext == "jpeg":
			stills[k] = append(stills[k], f)
		case ext == "mov":
			motions[k] = append(motions[k], f)
		}
	}

	var pairs []Pair
	for _, f := range files {
		ext := normalizeExt(f.Ext)
		if ext != "heic" && ext != "jpg" && ext != "jpeg" {
			continue
		}
		k := key{dir: filepath.Dir(f.Path), base: basenameNoExt(f.Path)}

		// Require a single still and a single motion for this key to keep the
		// mapping unambiguous.
		if len(stills[k]) != 1 || len(motions[k]) != 1 {
			continue
		}
		still := stills[k][0]
		motion := motions[k][0]

		// Guard against the same file appearing twice; iterate over the actual
		// still that owns this key.
		if still.Path != f.Path {
			continue
		}

		if !contentIdentifiersCompatible(still.ContentIdentifier, motion.ContentIdentifier) {
			continue
		}

		pairs = append(pairs, Pair{Still: still, Motion: motion})
	}
	return pairs
}

// contentIdentifiersCompatible reports whether two ContentIdentifier values may
// belong to the same Live Photo. When both are non-empty they must be equal;
// if either is empty, they are considered compatible (fall back to the basename
// rule).
func contentIdentifiersCompatible(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	return a == b
}

// basenameNoExt returns the file name of path with its extension removed.
func basenameNoExt(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
