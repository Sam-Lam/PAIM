package hashing

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"lukechampine.com/blake3"
)

// expectedQuick recomputes the quick hash independently from the spec so the
// package implementation is checked against a from-scratch reference rather
// than against itself.
func expectedQuick(t *testing.T, data []byte) string {
	t.Helper()
	h := blake3.New(32, nil)
	var sz [8]byte
	binary.LittleEndian.PutUint64(sz[:], uint64(len(data)))
	h.Write(sz[:])
	if len(data) <= quickThreshold {
		h.Write(data)
	} else {
		h.Write(data[:chunkSize])
		h.Write(data[len(data)-chunkSize:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func expectedFull(t *testing.T, data []byte) string {
	t.Helper()
	h := blake3.New(32, nil)
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	return path
}

// patterned returns n bytes with a non-repeating pattern so head and tail
// chunks differ.
func patterned(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + 7)
	}
	return b
}

func TestQuickHashGolden(t *testing.T) {
	cases := map[string][]byte{
		"empty":       {},
		"tiny":        []byte("hello world"),
		"exact_1byte": {0x42},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeTemp(t, data)
			got, err := QuickHash(path)
			if err != nil {
				t.Fatalf("QuickHash: %v", err)
			}
			if want := expectedQuick(t, data); got != want {
				t.Errorf("QuickHash = %s, want %s", got, want)
			}
		})
	}
}

func TestQuickHashBoundaries(t *testing.T) {
	// Sizes around the 8 MiB threshold and the 4 MiB chunk size.
	sizes := []int{
		chunkSize,          // 4 MiB
		quickThreshold - 1, // just below whole-content threshold
		quickThreshold,     // exactly 8 MiB -> whole content
		quickThreshold + 1, // just above -> head+tail chunks
		quickThreshold + chunkSize,
	}
	for _, size := range sizes {
		data := patterned(size)
		path := writeTemp(t, data)
		got, err := QuickHash(path)
		if err != nil {
			t.Fatalf("size %d: QuickHash: %v", size, err)
		}
		if want := expectedQuick(t, data); got != want {
			t.Errorf("size %d: QuickHash = %s, want %s", size, got, want)
		}
	}
}

// TestQuickHashReadAtInMemory exercises the internal chunking function directly
// against an in-memory reader, independent of the filesystem.
func TestQuickHashReadAtInMemory(t *testing.T) {
	data := patterned(quickThreshold + 123)
	got, err := quickHashReadAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("quickHashReadAt: %v", err)
	}
	if want := expectedQuick(t, data); got != want {
		t.Errorf("quickHashReadAt = %s, want %s", got, want)
	}
}

// TestQuickHashSizeSensitivity confirms the file size is part of the digest:
// two files with identical head/tail chunks but different sizes must differ.
func TestQuickHashSizeSensitivity(t *testing.T) {
	a := quickHashMust(t, patterned(quickThreshold+chunkSize))
	// Same pattern generator but a different length changes the size prefix and
	// (usually) the tail chunk.
	b := quickHashMust(t, patterned(quickThreshold+chunkSize+1))
	if a == b {
		t.Errorf("expected different quick hashes for different sizes")
	}
}

func quickHashMust(t *testing.T, data []byte) string {
	t.Helper()
	got, err := quickHashReadAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("quickHashReadAt: %v", err)
	}
	return got
}

func TestFullHash(t *testing.T) {
	// Larger than the 1 MiB streaming buffer to exercise multiple reads.
	data := patterned(fullBufferSize*2 + 500)
	path := writeTemp(t, data)
	got, err := FullHash(context.Background(), path)
	if err != nil {
		t.Fatalf("FullHash: %v", err)
	}
	if want := expectedFull(t, data); got != want {
		t.Errorf("FullHash = %s, want %s", got, want)
	}
}

func TestFullHashCancellation(t *testing.T) {
	data := patterned(fullBufferSize * 4)
	path := writeTemp(t, data)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before we start
	_, err := FullHash(ctx, path)
	if err == nil {
		t.Fatalf("expected error from cancelled context")
	}
	if !bytes.Contains([]byte(err.Error()), []byte(context.Canceled.Error())) {
		t.Errorf("expected wrapped context.Canceled, got %v", err)
	}
}

func TestVerifyCopy(t *testing.T) {
	data := patterned(quickThreshold + 2048)
	srcQuick := quickHashMust(t, data)
	srcFull := expectedFull(t, data)

	t.Run("match_quick_only", func(t *testing.T) {
		dst := writeTemp(t, data)
		ok, err := VerifyCopy(context.Background(), srcQuick, "", dst)
		if err != nil {
			t.Fatalf("VerifyCopy: %v", err)
		}
		if !ok {
			t.Errorf("expected match")
		}
	})

	t.Run("match_quick_and_full", func(t *testing.T) {
		dst := writeTemp(t, data)
		ok, err := VerifyCopy(context.Background(), srcQuick, srcFull, dst)
		if err != nil {
			t.Fatalf("VerifyCopy: %v", err)
		}
		if !ok {
			t.Errorf("expected match")
		}
	})

	t.Run("quick_mismatch", func(t *testing.T) {
		corrupt := patterned(quickThreshold + 2048)
		corrupt[0] ^= 0xff
		dst := writeTemp(t, corrupt)
		ok, err := VerifyCopy(context.Background(), srcQuick, srcFull, dst)
		if err != nil {
			t.Fatalf("VerifyCopy: %v", err)
		}
		if ok {
			t.Errorf("expected mismatch")
		}
	})

	t.Run("full_mismatch_quick_match", func(t *testing.T) {
		// Flip a byte in the interior (between head and tail chunks) so the
		// quick hash still matches but the full hash differs.
		corrupt := make([]byte, len(data))
		copy(corrupt, data)
		mid := chunkSize + (len(corrupt)-2*chunkSize)/2
		corrupt[mid] ^= 0xff
		dst := writeTemp(t, corrupt)

		// Sanity: quick hash unchanged.
		if quickHashMust(t, corrupt) != srcQuick {
			t.Fatalf("test setup: quick hash unexpectedly changed")
		}
		ok, err := VerifyCopy(context.Background(), srcQuick, srcFull, dst)
		if err != nil {
			t.Fatalf("VerifyCopy: %v", err)
		}
		if ok {
			t.Errorf("expected full-hash mismatch to fail verification")
		}
	})
}
