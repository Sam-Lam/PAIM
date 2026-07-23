package importer

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// entryByFilename indexes a plan's entries by basename for assertions.
func entryByFilename(plan *ReorganizePlan) map[string]ReorganizeEntry {
	byName := make(map[string]ReorganizeEntry, len(plan.Entries))
	for _, e := range plan.Entries {
		byName[e.Filename] = e
	}
	return byName
}

func assertToDir(t *testing.T, e ReorganizeEntry, wantDir string) {
	t.Helper()
	if got := filepath.Dir(e.To); got != wantDir {
		t.Errorf("%s destination dir = %q, want %q", e.Filename, got, wantDir)
	}
}

// TestReorganizePlanSourceFolderLabels covers capability 1: with
// UseSourceFolderLabels on and an empty EventName, each file's label comes from
// its current parent folder (via archive.DeriveLabel). Real labels are kept;
// generic camera dirs and pure dates are excluded (bare date folder); a
// "YYYY-MM-DD Label" parent contributes only its label part.
func TestReorganizePlanSourceFolderLabels(t *testing.T) {
	h := newReorgHarness(t)
	yosemiteDate := time.Date(2019, 6, 12, 9, 0, 0, 0, time.Local)

	h.write(filepath.Join("old memories", "keep.jpg"), "keep-bytes", reorg2023)
	h.write(filepath.Join("DCIM", "cam.jpg"), "cam-bytes", reorg2023)
	h.write(filepath.Join("100_FUJI", "fuji.jpg"), "fuji-bytes", reorg2023)
	h.write(filepath.Join("2019-06-12 Yosemite", "yos.jpg"), "yos-bytes", yosemiteDate)
	h.adopt()

	plan, err := h.pipe.PlanReorganize(context.Background(), ReorganizeOptions{UseSourceFolderLabels: true}, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	byName := entryByFilename(plan)

	// A plain parent folder becomes the event label.
	assertToDir(t, byName["keep.jpg"], filepath.Join(h.root, "2023", "2023-06-15 old memories"))
	// Generic camera dirs are excluded → bare date folder.
	assertToDir(t, byName["cam.jpg"], filepath.Join(h.root, "2023", "2023-06-15"))
	assertToDir(t, byName["fuji.jpg"], filepath.Join(h.root, "2023", "2023-06-15"))
	// A "YYYY-MM-DD Label" parent contributes only its label part.
	assertToDir(t, byName["yos.jpg"], filepath.Join(h.root, "2019", "2019-06-12 Yosemite"))
}

// TestReorganizePlanExplicitEventBeatsLabels covers the interplay rule: a
// non-empty EventName always wins over derived labels, targeting
// "YYYY-MM-DD EventName" exactly for every file.
func TestReorganizePlanExplicitEventBeatsLabels(t *testing.T) {
	h := newReorgHarness(t)
	h.write(filepath.Join("old memories", "keep.jpg"), "keep-bytes", reorg2023)
	h.write(filepath.Join("DCIM", "cam.jpg"), "cam-bytes", reorg2023)
	h.adopt()

	plan, err := h.pipe.PlanReorganize(context.Background(),
		ReorganizeOptions{EventName: "Override", UseSourceFolderLabels: true}, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	byName := entryByFilename(plan)
	want := filepath.Join(h.root, "2023", "2023-06-15 Override")
	assertToDir(t, byName["keep.jpg"], want)
	assertToDir(t, byName["cam.jpg"], want)
}

// TestReorganizePlanLabelsOffUnchanged proves the engine default (labels off)
// keeps every empty-event file in a bare date folder — the pre-existing behavior
// that e2e and existing tests rely on.
func TestReorganizePlanLabelsOffUnchanged(t *testing.T) {
	h := newReorgHarness(t)
	h.write(filepath.Join("old memories", "keep.jpg"), "keep-bytes", reorg2023)
	h.adopt()

	plan, err := h.pipe.PlanReorganize(context.Background(), ReorganizeOptions{}, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	byName := entryByFilename(plan)
	assertToDir(t, byName["keep.jpg"], filepath.Join(h.root, "2023", "2023-06-15"))
}

// TestReorganizePlanStickyJoinsExistingLabeledFolder covers capability 3 at the
// reorganize layer: with an empty event and labels OFF, a loose file whose date
// already has exactly one labeled folder on disk joins that folder instead of
// creating a bare sibling.
func TestReorganizePlanStickyJoinsExistingLabeledFolder(t *testing.T) {
	h := newReorgHarness(t)
	// An already-organized labeled folder for the date exists on disk...
	h.write(filepath.Join("2023", "2023-06-15 Yosemite", "settled.jpg"), "settled-bytes", reorg2023)
	// ...and a loose file of the same date needs a home.
	h.write("loose.jpg", "loose-bytes", reorg2023)
	h.adopt()

	plan, err := h.pipe.PlanReorganize(context.Background(), ReorganizeOptions{}, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	byName := entryByFilename(plan)
	// settled.jpg is already inside the labeled folder → in place.
	if e := byName["settled.jpg"]; e.Kind != MoveInPlace {
		t.Errorf("settled.jpg kind = %q, want in_place", e.Kind)
	}
	// loose.jpg sticks to the single existing "2023-06-15*" folder.
	assertToDir(t, byName["loose.jpg"], filepath.Join(h.root, "2023", "2023-06-15 Yosemite"))
}
