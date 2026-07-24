// Package mediatype is the registry of file extensions PAIM understands and the
// rules for classifying them into media kinds. It also implements Live Photo
// pairing, matching a still (HEIC or JPG) with its companion MOV.
//
// Classification is case-insensitive and tolerant of a leading dot, so "JPG",
// ".jpg" and "jpg" are equivalent.
package mediatype

import (
	"path/filepath"
	"sort"
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

// Scope tokens are the media-kind selectors a backup provider's MediaScope
// enables ({photos,videos,raws}). They map onto Kind: photos<-Photo,
// videos<-Video, raws<-RawPhoto. An empty scope means "all kinds" (the default,
// backward compatible with providers created before per-provider scoping).
const (
	ScopePhotos = "photos"
	ScopeVideos = "videos"
	ScopeRaws   = "raws"
)

// AllScopes is the canonical, ordered set of scope tokens. It is the "everything"
// scope an empty MediaScope is equivalent to.
var AllScopes = []string{ScopePhotos, ScopeVideos, ScopeRaws}

// scopeForKind maps a media Kind to its scope token. Unknown (a non-media
// extension) has no scope token and returns "".
func scopeForKind(k Kind) string {
	switch k {
	case Photo:
		return ScopePhotos
	case Video:
		return ScopeVideos
	case RawPhoto:
		return ScopeRaws
	}
	return ""
}

// ScopeIncludes reports whether a provider scope (a CSV of scope tokens, e.g.
// "photos,videos") includes the media kind of ext. The mapping is KindOf(ext):
// Photo->photos, Video->videos, RawPhoto->raws.
//
//   - An empty (or whitespace-only) scope means "all kinds" and includes every
//     media extension — the default that keeps pre-scoping providers unchanged.
//   - An Unknown extension (not in the registry) has no kind to scope on and is
//     therefore NEVER included by a non-empty scope; under the empty ("all") scope
//     it is likewise not a media file the caller would enqueue. Documented so the
//     caller does not mistake "not scoped" for "excluded by policy".
//
// This is the single scoping key. It is deliberately driven by the FILE's
// extension, not by the asset's MediaType, because the two components of a Live
// Photo pair share MediaType live_photo_pair yet must be judged independently: the
// still (heic/jpg) counts as photos, its companion MOV counts as videos.
func ScopeIncludes(scope, ext string) bool {
	tok := scopeForKind(KindOf(ext))
	if tok == "" {
		return false // Unknown extension: no kind, so not in any scope
	}
	if strings.TrimSpace(scope) == "" {
		return true // empty scope = all kinds
	}
	for _, s := range strings.Split(scope, ",") {
		if strings.TrimSpace(s) == tok {
			return true
		}
	}
	return false
}

// EnabledScopes parses a MediaScope CSV into the set of scope tokens it enables,
// in AllScopes order. An empty/whitespace scope enables every kind (returns a copy
// of AllScopes); unrecognized tokens are ignored.
func EnabledScopes(scope string) []string {
	if strings.TrimSpace(scope) == "" {
		return append([]string(nil), AllScopes...)
	}
	set := make(map[string]bool)
	for _, s := range strings.Split(scope, ",") {
		set[strings.TrimSpace(s)] = true
	}
	out := make([]string, 0, len(AllScopes))
	for _, k := range AllScopes {
		if set[k] {
			out = append(out, k)
		}
	}
	return out
}

// NormalizeScope canonicalizes a MediaScope: it re-serializes the enabled tokens
// in AllScopes order and collapses a full ("all kinds") scope back to the empty
// string, so the stored default value is uniform and backward compatible.
// Unrecognized tokens are dropped.
func NormalizeScope(scope string) string {
	scopes := EnabledScopes(scope)
	if len(scopes) == len(AllScopes) {
		return ""
	}
	return strings.Join(scopes, ",")
}

// ExtensionsForScopes returns the sorted, normalized (lowercase, no dot)
// extensions belonging to the given scope tokens, drawn from the fixed registry.
// Because the result derives only from the built-in registry (never caller text),
// it is safe to interpolate into a SQL IN() predicate.
func ExtensionsForScopes(scopes []string) []string {
	want := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		want[s] = true
	}
	var out []string
	for ext, k := range registry {
		if want[scopeForKind(k)] {
			out = append(out, ext)
		}
	}
	sort.Strings(out)
	return out
}

// ScopedExtensions returns the extension whitelist a MediaScope restricts to, or
// nil when the scope imposes NO restriction (empty scope, or a scope that enables
// every kind). A non-nil, possibly-empty slice means "restrict to exactly these
// extensions" — callers build an `IN (?)` predicate from it and treat nil as "no
// extension filter" (all eligible rows). Efficient for large catalogs: the filter
// runs in SQL rather than loading out-of-scope rows into Go.
func ScopedExtensions(scope string) []string {
	scopes := EnabledScopes(scope)
	if len(scopes) == len(AllScopes) {
		return nil // all kinds -> no restriction
	}
	return ExtensionsForScopes(scopes)
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
