package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
)

// fakeLookup maps quick hash -> archived assets.
type fakeLookup struct {
	byQuick map[string][]ArchivedAsset
	err     error
}

func (l fakeLookup) FindByQuickHash(_ context.Context, quick string) ([]ArchivedAsset, error) {
	if l.err != nil {
		return nil, l.err
	}
	return l.byQuick[quick], nil
}

// quickOf and fullOf replicate fakeHasher's hashing over raw content so tests
// can predict the hash a file with that content will produce.
func quickOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "q:" + hex.EncodeToString(sum[:8])
}

func fullOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "f:" + hex.EncodeToString(sum[:])
}

func newIdentifierForErase() *Identifier {
	return NewIdentifier(nil, nil, fakeHasher{}, nil)
}

func TestEvaluateSafeToErase_AllArchivedSafe(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.jpg"), "one")
	writeFile(t, filepath.Join(root, "b.cr3"), "two")

	lookup := fakeLookup{byQuick: map[string][]ArchivedAsset{
		quickOf("one"): {{ID: "a", QuickHash: quickOf("one"), Verified: true, BackupComplete: true}},
		quickOf("two"): {{ID: "b", QuickHash: quickOf("two"), Verified: true, BackupComplete: true}},
	}}

	id := newIdentifierForErase()
	rep, err := id.EvaluateSafeToErase(context.Background(), "src-1", root, lookup, isMediaTest)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Safe {
		t.Errorf("Safe = false, want true (reason: %s)", rep.Reason)
	}
	if rep.Archived != 2 || rep.TotalMedia != 2 {
		t.Errorf("Archived=%d TotalMedia=%d, want 2/2", rep.Archived, rep.TotalMedia)
	}
	if rep.New != 0 || rep.Unverified != 0 || rep.BackupIncomplete != 0 {
		t.Errorf("unexpected problem counts: %+v", rep)
	}
}

func TestEvaluateSafeToErase_NewFileUnsafe(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.jpg"), "one")
	writeFile(t, filepath.Join(root, "new.jpg"), "brand-new") // not in archive

	lookup := fakeLookup{byQuick: map[string][]ArchivedAsset{
		quickOf("one"): {{ID: "a", Verified: true, BackupComplete: true}},
	}}

	id := newIdentifierForErase()
	rep, err := id.EvaluateSafeToErase(context.Background(), "src-1", root, lookup, isMediaTest)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Safe {
		t.Error("Safe = true, want false")
	}
	if rep.New != 1 {
		t.Errorf("New = %d, want 1", rep.New)
	}
	if !contains(rep.Reason, "not yet imported") {
		t.Errorf("reason %q should mention not-yet-imported", rep.Reason)
	}
}

func TestEvaluateSafeToErase_UnverifiedUnsafe(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.jpg"), "one")

	lookup := fakeLookup{byQuick: map[string][]ArchivedAsset{
		quickOf("one"): {{ID: "a", Verified: false, BackupComplete: true}}, // matched but not verified
	}}

	id := newIdentifierForErase()
	rep, err := id.EvaluateSafeToErase(context.Background(), "src-1", root, lookup, isMediaTest)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Safe {
		t.Error("Safe = true, want false")
	}
	if rep.Unverified != 1 {
		t.Errorf("Unverified = %d, want 1", rep.Unverified)
	}
	if !contains(rep.Reason, "not verified") {
		t.Errorf("reason %q should mention verification", rep.Reason)
	}
}

func TestEvaluateSafeToErase_BackupIncompleteUnsafe(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.jpg"), "one")

	lookup := fakeLookup{byQuick: map[string][]ArchivedAsset{
		quickOf("one"): {{ID: "a", Verified: true, BackupComplete: false}},
	}}

	id := newIdentifierForErase()
	rep, err := id.EvaluateSafeToErase(context.Background(), "src-1", root, lookup, isMediaTest)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Safe {
		t.Error("Safe = true, want false")
	}
	if rep.BackupIncomplete != 1 {
		t.Errorf("BackupIncomplete = %d, want 1", rep.BackupIncomplete)
	}
	if !contains(rep.Reason, "incomplete backups") {
		t.Errorf("reason %q should mention backups", rep.Reason)
	}
}

// TestEvaluateSafeToErase_QuickHashCollisionResolvedByFullHash covers the case
// where several archived assets share a quick hash and the file's full hash
// disambiguates.
func TestEvaluateSafeToErase_QuickHashCollisionResolvedByFullHash(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.jpg"), "collide")

	h := fakeHasher{}
	quick := quickOf("collide")
	full := fullOf("collide")

	lookup := fakeLookup{byQuick: map[string][]ArchivedAsset{
		quick: {
			{ID: "other", QuickHash: quick, FullHash: "f:not-this-one", Verified: true, BackupComplete: true},
			{ID: "match", QuickHash: quick, FullHash: full, Verified: true, BackupComplete: true},
		},
	}}

	id := NewIdentifier(nil, nil, h, nil)
	rep, err := id.EvaluateSafeToErase(context.Background(), "src-1", root, lookup, isMediaTest)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Safe {
		t.Errorf("Safe = false, want true (collision should resolve): %+v", rep)
	}
	if rep.Archived != 1 {
		t.Errorf("Archived = %d, want 1", rep.Archived)
	}
}

func TestEvaluateSafeToErase_EmptyVolumeSafe(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "readme.txt"), "no media here")
	id := newIdentifierForErase()
	rep, err := id.EvaluateSafeToErase(context.Background(), "src-1", root, fakeLookup{}, isMediaTest)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Safe {
		t.Errorf("Safe = false, want true for no-media volume")
	}
	if rep.TotalMedia != 0 {
		t.Errorf("TotalMedia = %d, want 0", rep.TotalMedia)
	}
}

// helpers ---------------------------------------------------------------------

func contains(s, sub string) bool { return len(s) >= len(sub) && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
