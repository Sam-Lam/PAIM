package services

import (
	"testing"

	"github.com/Sam-Lam/PAIM/internal/domain"
)

func TestToAssetDTODuplicatePointer(t *testing.T) {
	orig := "orig-123"
	a := domain.Asset{
		UUIDModel:          domain.UUIDModel{ID: "dup-1"},
		OriginalFilename:   "IMG_0001.HEIC",
		MediaType:          domain.MediaTypePhoto,
		VerificationStatus: domain.VerificationStatusVerified,
		BackupStatus:       domain.BackupStatusComplete,
		DuplicateOfAssetID: &orig,
	}
	dto := toAssetDTO(a, "")
	if dto.ID != "dup-1" || dto.MediaType != "photo" {
		t.Fatalf("unexpected mapping: %+v", dto)
	}
	if dto.DuplicateOfAssetID != "orig-123" {
		t.Fatalf("duplicate pointer not flattened: %q", dto.DuplicateOfAssetID)
	}

	a.DuplicateOfAssetID = nil
	if got := toAssetDTO(a, "").DuplicateOfAssetID; got != "" {
		t.Fatalf("nil duplicate should map to empty string, got %q", got)
	}
}

func TestToSessionDTOModeFromNotes(t *testing.T) {
	notes := resumeState{Mode: "adopt", SourceRoot: "/vol/src"}.encode()
	s := domain.ImportSession{
		UUIDModel: domain.UUIDModel{ID: "sess-1"},
		Status:    domain.SessionStatusCompleted,
		Notes:     notes,
	}
	dto := toSessionDTO(s)
	if dto.Mode != "adopt" {
		t.Fatalf("mode not decoded from notes: %q", dto.Mode)
	}
	if dto.Status != "completed" {
		t.Fatalf("status mapping wrong: %q", dto.Status)
	}

	// Empty/unparseable notes default to copy.
	s.Notes = ""
	if got := toSessionDTO(s).Mode; got != "copy" {
		t.Fatalf("empty notes should default mode to copy, got %q", got)
	}
}

func TestSummaryFromCounts(t *testing.T) {
	statuses := []domain.JobStatus{
		domain.JobStatusPending,
		domain.JobStatusFailed,
		domain.JobStatusCompleted,
	}
	values := []int64{3, 2, 5}
	got := summaryFromCounts(statuses, values)
	if got.Pending != 3 || got.Failed != 2 || got.Completed != 5 {
		t.Fatalf("unexpected summary: %+v", got)
	}
	if got.Total != 10 {
		t.Fatalf("total = %d want 10", got.Total)
	}
}

func TestNormalizePage(t *testing.T) {
	// 1-based: page 1 is the first page (offset 0), page 2 skips one page.
	if l, o := normalizePage(1, 25); l != 25 || o != 0 {
		t.Fatalf("normalizePage(1,25) = %d,%d want 25,0", l, o)
	}
	limit, offset := normalizePage(2, 25)
	if limit != 25 || offset != 25 {
		t.Fatalf("normalizePage(2,25) = %d,%d want 25,25", limit, offset)
	}
	// page<=0 clamps to the first page; page size defaults when non-positive.
	if l, o := normalizePage(-1, 0); l != 50 || o != 0 {
		t.Fatalf("normalizePage(-1,0) = %d,%d want 50,0", l, o)
	}
	if l, o := normalizePage(0, 10000); l != 500 || o != 0 {
		t.Fatalf("normalizePage(0,10000) = %d,%d want 500,0", l, o)
	}
}
