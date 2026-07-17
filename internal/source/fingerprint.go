// Package source identifies the origin of imported media (an SD card, USB SSD,
// external HDD, internal folder, or network share) and scores how confident that
// identification is, with a human-readable reason for every conclusion. It also
// computes a content fingerprint used to recognise a volume whose hardware
// identity is unavailable, and evaluates whether a volume is safe to erase.
//
// Identity is never taken from a volume label (labels such as UNTITLED, NO NAME
// or EOS_DIGITAL recur across unrelated media). Labels are stored for display
// only; matching uses hardware serials, volume/filesystem UUIDs, and the content
// fingerprint.
package source

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"lukechampine.com/blake3"
)

const (
	// pathSampleLimit is the maximum number of evenly-sampled media files whose
	// relative paths form the representative path hash.
	pathSampleLimit = 200
	// contentSampleLimit is the maximum number of evenly-sampled media files
	// whose quick hashes form the representative content hash.
	contentSampleLimit = 20
	// progressInterval is how often (in scanned files) progressFn is invoked.
	progressInterval = 128
)

// FileHasher provides the quick hash of a file. It is injected (wired in main.go
// to internal/hashing) so this package does not depend on the hashing engine.
type FileHasher interface {
	// QuickHash returns the hex BLAKE3 quick hash (size + head/tail) of path.
	QuickHash(path string) (string, error)
}

// Fingerprint is a content-based signature of a volume's media, used to
// recognise previously-seen contents even when hardware identity is missing. It
// is stored as JSON in domain.ImportSource.ContentFingerprint.
type Fingerprint struct {
	// TotalFileCount is the number of media files found under the root.
	TotalFileCount int `json:"totalFileCount"`
	// TotalBytes is the summed size of those media files.
	TotalBytes int64 `json:"totalBytes"`
	// PathHash is the hex BLAKE3 of the sorted relative paths of up to
	// pathSampleLimit evenly-sampled media files.
	PathHash string `json:"pathHash"`
	// ContentHash is the hex BLAKE3 over the quick hashes (ordered by path) of up
	// to contentSampleLimit evenly-sampled media files.
	ContentHash string `json:"contentHash"`
}

// JSON serialises the fingerprint for storage. It never fails for this
// fixed-shape struct; on the impossible marshal error it returns "{}".
func (f Fingerprint) JSON() string {
	b, err := json.Marshal(f)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ParseFingerprint decodes a stored fingerprint. An empty string yields a zero
// Fingerprint and no error.
func ParseFingerprint(s string) (Fingerprint, error) {
	var f Fingerprint
	if strings.TrimSpace(s) == "" {
		return f, nil
	}
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return f, fmt.Errorf("source: parse fingerprint: %w", err)
	}
	return f, nil
}

// ComputeFingerprint walks root recursively and builds a Fingerprint from the
// media files found. Hidden directories (names beginning with ".") are skipped,
// as are non-media files (per isMedia, which receives a lowercase extension with
// no leading dot). The walk honours ctx between entries and reports progress via
// progressFn (which may be nil).
//
// Determinism: the same tree always yields the same hashes; adding or removing a
// media file changes TotalFileCount/TotalBytes and, in general, PathHash.
func ComputeFingerprint(
	ctx context.Context,
	root string,
	hasher FileHasher,
	isMedia func(ext string) bool,
	progressFn func(scanned int),
) (*Fingerprint, error) {
	if isMedia == nil {
		return nil, fmt.Errorf("source: compute fingerprint %q: isMedia is nil", root)
	}

	type mediaFile struct {
		rel  string
		size int64
	}
	var files []mediaFile
	scanned := 0

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A single unreadable entry must not abort fingerprinting; skip it.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		name := d.Name()
		if d.IsDir() {
			// Skip hidden directories (but never the root itself).
			if path != root && strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		ext := normaliseExt(name)
		if !isMedia(ext) {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		var size int64
		if info, statErr := d.Info(); statErr == nil {
			size = info.Size()
		}
		files = append(files, mediaFile{rel: filepath.ToSlash(rel), size: size})

		scanned++
		if progressFn != nil && scanned%progressInterval == 0 {
			progressFn(scanned)
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("source: walk %q for fingerprint: %w", root, walkErr)
	}
	if progressFn != nil {
		progressFn(scanned)
	}

	fp := &Fingerprint{TotalFileCount: len(files)}
	for _, f := range files {
		fp.TotalBytes += f.size
	}

	// Sort by relative path for deterministic sampling and hashing.
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })

	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.rel
	}

	// Path hash: hex BLAKE3 of the newline-joined sorted relative paths of an
	// even sample.
	pathSample := evenSampleStrings(paths, pathSampleLimit)
	fp.PathHash = hashStrings(pathSample)

	// Content hash: quick-hash an even sample and hash the concatenation (ordered
	// by path). A hasher error on one file degrades to skipping that file rather
	// than failing the whole fingerprint.
	contentIdx := evenSampleIndexes(len(files), contentSampleLimit)
	quickHashes := make([]string, 0, len(contentIdx))
	for _, idx := range contentIdx {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("source: fingerprint %q cancelled: %w", root, ctxErr)
		}
		abs := filepath.Join(root, filepath.FromSlash(files[idx].rel))
		qh, err := hasher.QuickHash(abs)
		if err != nil {
			continue
		}
		quickHashes = append(quickHashes, qh)
	}
	fp.ContentHash = hashStrings(quickHashes)

	return fp, nil
}

// normaliseExt returns the lowercase extension of name without the leading dot.
func normaliseExt(name string) string {
	ext := filepath.Ext(name)
	return strings.ToLower(strings.TrimPrefix(ext, "."))
}

// hashStrings returns the hex BLAKE3 of the newline-joined input. An empty input
// hashes the empty byte slice, giving a stable non-empty digest.
func hashStrings(items []string) string {
	h := blake3.New(32, nil)
	for i, s := range items {
		if i > 0 {
			_, _ = h.Write([]byte{'\n'})
		}
		_, _ = h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// evenSampleStrings returns up to k elements of items, evenly spaced. items is
// assumed already sorted; the result preserves order.
func evenSampleStrings(items []string, k int) []string {
	idx := evenSampleIndexes(len(items), k)
	out := make([]string, len(idx))
	for i, j := range idx {
		out[i] = items[j]
	}
	return out
}

// evenSampleIndexes returns up to k indexes into a slice of length n, evenly
// spaced across [0, n). When n <= k every index is returned.
func evenSampleIndexes(n, k int) []int {
	if n <= 0 || k <= 0 {
		return nil
	}
	if n <= k {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	out := make([]int, k)
	// Evenly spread k picks across n items.
	for i := 0; i < k; i++ {
		out[i] = i * n / k
	}
	return out
}
