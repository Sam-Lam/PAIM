package source

import (
	"context"
	"path/filepath"
	"testing"
)

// TestEvaluateSafeToErase_Progress verifies the two-phase evaluation reports a
// determinate FilesTotal from the first callback (enumeration completes before
// hashing) and advances FilesDone to the total, ending on a final full-progress
// callback.
func TestEvaluateSafeToErase_Progress(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.jpg"), "one")
	writeFile(t, filepath.Join(root, "b.cr3"), "two")
	writeFile(t, filepath.Join(root, "c.mov"), "three")
	writeFile(t, filepath.Join(root, "notes.txt"), "ignored") // non-media, not counted

	lookup := fakeLookup{byQuick: map[string][]ArchivedAsset{
		quickOf("one"):   {{ID: "a", QuickHash: quickOf("one"), Verified: true, BackupComplete: true}},
		quickOf("two"):   {{ID: "b", QuickHash: quickOf("two"), Verified: true, BackupComplete: true}},
		quickOf("three"): {{ID: "c", QuickHash: quickOf("three"), Verified: true, BackupComplete: true}},
	}}

	id := newIdentifierForErase()

	type sample struct {
		done, total int
	}
	var samples []sample
	rep, err := id.EvaluateSafeToErase(context.Background(), "src-1", root, true, lookup, isMediaTest, func(done, total int, _ string) {
		samples = append(samples, sample{done, total})
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.TotalMedia != 3 {
		t.Fatalf("TotalMedia = %d, want 3", rep.TotalMedia)
	}
	if len(samples) == 0 {
		t.Fatal("no progress callbacks")
	}
	// Every callback must carry the known total of 3 (determinate from the start).
	for i, s := range samples {
		if s.total != 3 {
			t.Fatalf("sample %d total = %d, want 3", i, s.total)
		}
	}
	// The final callback reports completion.
	last := samples[len(samples)-1]
	if last.done != 3 || last.total != 3 {
		t.Fatalf("final sample = %+v, want {3 3}", last)
	}
}

// TestEvaluateSafeToErase_Cancel verifies a cancelled context aborts the
// evaluation with a context error rather than returning a partial report.
func TestEvaluateSafeToErase_Cancel(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.jpg", "b.jpg", "c.jpg", "d.jpg"} {
		writeFile(t, filepath.Join(root, n), n)
	}
	lookup := fakeLookup{byQuick: map[string][]ArchivedAsset{}}

	id := newIdentifierForErase()
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel from within the first progress callback so the hashing loop observes
	// the cancellation between files.
	_, err := id.EvaluateSafeToErase(ctx, "src-1", root, true, lookup, isMediaTest, func(done, total int, _ string) {
		cancel()
	})
	if err == nil {
		t.Fatal("expected a cancellation error, got nil")
	}
	if ctx.Err() == nil {
		t.Fatal("context was not cancelled")
	}
}
