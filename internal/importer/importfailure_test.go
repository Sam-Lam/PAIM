package importer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/repo"
)

// TestImportRecordsStructuredFailure forces a per-file verification failure and
// asserts the pipeline records a structured ImportFailure row (open, with the
// source path and a stage) in addition to bumping the session Failures counter.
func TestImportRecordsStructuredFailure(t *testing.T) {
	h := newHarness(t)
	path := h.writeFile("corruptme.jpg", "original-good-content", testDate)

	// Corrupt the partial after copy so verification fails deterministically.
	h.pipe.afterCopyHook = func(partialPath string) {
		_ = os.WriteFile(partialPath, []byte("corrupted-different!!"), 0o644)
	}

	session, err := h.pipe.Run(context.Background(), h.copyOpts(), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if session.Failures != 1 {
		t.Fatalf("Failures counter = %d, want 1", session.Failures)
	}

	rows, total, err := h.failures.ListForSession(context.Background(), session.ID, repo.Page{})
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("failure rows: total=%d len=%d, want 1/1", total, len(rows))
	}
	f := rows[0]
	if f.Status != domain.ImportFailureStatusOpen {
		t.Fatalf("failure status = %q, want open", f.Status)
	}
	if f.Path != path {
		t.Fatalf("failure path = %q, want %q", f.Path, path)
	}
	if f.Op != domain.ImportFailureOpVerify {
		t.Fatalf("failure op = %q, want verify", f.Op)
	}
	if f.ErrorMessage == "" {
		t.Fatal("failure error message is empty")
	}
	if f.ResolvedAt != nil {
		t.Fatal("open failure should have no ResolvedAt")
	}
}

// TestRetryFileSucceedsAfterFix imports a file that fails verification (recording
// a failure), then — with the corruption hook removed — retries just that file
// through RetryFile and asserts it is resolved and the asset is now recorded.
func TestRetryFileSucceedsAfterFix(t *testing.T) {
	h := newHarness(t)
	path := h.writeFile("photo.jpg", "good-content-here", testDate)

	corrupt := true
	h.pipe.afterCopyHook = func(partialPath string) {
		if corrupt {
			_ = os.WriteFile(partialPath, []byte("nope"), 0o644)
		}
	}

	session, err := h.pipe.Run(context.Background(), h.copyOpts(), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if session.FilesImported != 0 || session.Failures != 1 {
		t.Fatalf("after first run: imported=%d failures=%d, want 0/1", session.FilesImported, session.Failures)
	}

	// Stop corrupting: the retry copies + verifies cleanly.
	corrupt = false
	outcome, err := h.pipe.RetryFile(context.Background(), session.ID, h.copyOpts(), path)
	if err != nil {
		t.Fatalf("RetryFile: %v", err)
	}
	if !outcome.Resolved || outcome.Failed {
		t.Fatalf("outcome = %+v, want resolved", outcome)
	}
	if outcome.AssetID == "" {
		t.Fatal("resolved retry should have created an asset")
	}
	if got := h.countAssets(); got != 1 {
		t.Fatalf("assets = %d, want 1 after retry", got)
	}
	mustExist(t, filepath.Join(h.destRoot, "2023", "2023-06-15 Trip", "photo.jpg"))
}
