// Package hashing implements PAIM's content-identity hashing using BLAKE3.
//
// Two hash strategies are provided:
//
//   - QuickHash: a cheap fingerprint over the file size plus the first and last
//     4 MiB of content (or the entire content for small files). It is used for
//     Stage 1 duplicate detection and copy verification.
//   - FullHash: a streamed BLAKE3 of the complete file content, used to confirm
//     identity once a quick-hash collision is found and for verification.
//
// Both produce lowercase hex-encoded digests. All hashing honors context
// cancellation where a context is accepted, and errors are wrapped with the
// offending path and operation name.
package hashing

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"lukechampine.com/blake3"
)

const (
	// chunkSize is the number of bytes read from the head and tail of a large
	// file for the quick hash (4 MiB).
	chunkSize = 4 << 20

	// quickThreshold is the size at or below which the quick hash covers the
	// entire file content (8 MiB) instead of head+tail chunks.
	quickThreshold = 8 << 20

	// fullBufferSize is the streaming buffer used by FullHash (1 MiB).
	fullBufferSize = 1 << 20
)

// QuickHash computes the quick fingerprint of the file at path.
//
// The digest is BLAKE3 over the 8-byte little-endian file size followed by the
// first 4 MiB and last 4 MiB of content. For files of 8 MiB or smaller the
// entire content is used in place of the head/tail chunks. The result is
// lowercase hex-encoded.
func QuickHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("quick hash: open %q: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("quick hash: stat %q: %w", path, err)
	}

	sum, err := quickHashReadAt(f, info.Size())
	if err != nil {
		return "", fmt.Errorf("quick hash: read %q: %w", path, err)
	}
	return sum, nil
}

// quickHashReadAt computes the quick hash from an io.ReaderAt of known size. It
// is separated from QuickHash so the chunking logic can be exercised against an
// in-memory implementation in tests.
func quickHashReadAt(r io.ReaderAt, size int64) (string, error) {
	h := blake3.New(32, nil)

	var sizeBuf [8]byte
	binary.LittleEndian.PutUint64(sizeBuf[:], uint64(size))
	// Writing to a blake3.Hasher never returns an error, but check defensively.
	if _, err := h.Write(sizeBuf[:]); err != nil {
		return "", err
	}

	if size <= quickThreshold {
		if err := copySection(h, r, 0, size); err != nil {
			return "", err
		}
	} else {
		if err := copySection(h, r, 0, chunkSize); err != nil {
			return "", err
		}
		if err := copySection(h, r, size-chunkSize, chunkSize); err != nil {
			return "", err
		}
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// copySection copies exactly length bytes starting at offset from r into w.
func copySection(w io.Writer, r io.ReaderAt, offset, length int64) error {
	if length <= 0 {
		return nil
	}
	sr := io.NewSectionReader(r, offset, length)
	if _, err := io.Copy(w, sr); err != nil {
		return fmt.Errorf("read %d bytes at offset %d: %w", length, offset, err)
	}
	return nil
}

// FullHash computes the streamed BLAKE3 digest of the entire file at path using
// a 1 MiB buffer. Context cancellation is honored between reads: if ctx is done
// the operation stops and returns ctx.Err(). The result is lowercase
// hex-encoded.
func FullHash(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("full hash: open %q: %w", path, err)
	}
	defer f.Close()

	sum, err := fullHashReader(ctx, f)
	if err != nil {
		return "", fmt.Errorf("full hash: read %q: %w", path, err)
	}
	return sum, nil
}

// fullHashReader streams r through BLAKE3 with a 1 MiB buffer, checking ctx
// between reads. It is separated from FullHash for testability.
func fullHashReader(ctx context.Context, r io.Reader) (string, error) {
	h := blake3.New(32, nil)
	buf := make([]byte, fullBufferSize)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := h.Write(buf[:n]); werr != nil {
				return "", werr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyCopy re-hashes the destination file at dstPath using the same
// strategies as the source and compares the results. The quick hash is always
// compared. The full hash is compared only when srcFullHash is non-empty (i.e.
// it was computed for the source). It returns true only when every compared
// hash matches.
func VerifyCopy(ctx context.Context, srcQuickHash, srcFullHash, dstPath string) (bool, error) {
	dstQuick, err := QuickHash(dstPath)
	if err != nil {
		return false, fmt.Errorf("verify copy %q: %w", dstPath, err)
	}
	if dstQuick != srcQuickHash {
		return false, nil
	}

	if srcFullHash != "" {
		dstFull, err := FullHash(ctx, dstPath)
		if err != nil {
			return false, fmt.Errorf("verify copy %q: %w", dstPath, err)
		}
		if dstFull != srcFullHash {
			return false, nil
		}
	}

	return true, nil
}
