package importer

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/Sam-Lam/PAIM/internal/archive"
)

// captureHandler is a slog.Handler that records every emitted Record so a test
// can assert which attributes per-file import logs carry. It intentionally keeps
// only record-level attrs (the sessionId is passed at each call site, so it lands
// in the Record, not in a WithAttrs handler chain).
type captureHandler struct {
	mu      *sync.Mutex
	records *[]slog.Record
}

func (h captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, r.Clone())
	return nil
}

func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h captureHandler) WithGroup(string) slog.Handler      { return h }

// attrValue returns the string value of the record's attr with the given key, or
// "" when absent.
func attrValue(r slog.Record, key string) string {
	out := ""
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			out = a.Value.String()
			return false
		}
		return true
	})
	return out
}

// hasAttr reports whether the record carries an attr with the given key.
func hasAttr(r slog.Record, key string) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			found = true
			return false
		}
		return true
	})
	return found
}

func (h *harness) withCapturingLogger(t *testing.T) *[]slog.Record {
	t.Helper()
	var recs []slog.Record
	logger := slog.New(captureHandler{mu: &sync.Mutex{}, records: &recs})
	h.pipe = New(Config{
		DB:        h.db,
		Assets:    h.assets,
		Sessions:  h.sessions,
		Extractor: h.extractor,
		Layout:    archive.New(h.destRoot),
		Backup:    h.enqueuer,
		Logger:    logger,
	})
	return &recs
}

// TestImportPerFileLogsCarrySessionID verifies the fix for empty Session Events:
// every per-file import log line stamps the session ID, so History's per-session
// drill-down (which matches logs by sessionId) finds them.
func TestImportPerFileLogsCarrySessionID(t *testing.T) {
	h := newHarness(t)
	recs := h.withCapturingLogger(t)

	h.writeFile("IMG_0001.jpg", "content-one", testDate)
	h.writeFile("IMG_0002.jpg", "content-two", testDate)

	session, err := h.pipe.Run(context.Background(), h.copyOpts(), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if session.FilesImported != 2 {
		t.Fatalf("FilesImported = %d, want 2", session.FilesImported)
	}

	// The per-file "imported (copy, verified)" line must carry the sessionId.
	imported := 0
	for _, r := range *recs {
		if r.Message == "imported (copy, verified)" {
			imported++
			if got := attrValue(r, "sessionId"); got != session.ID {
				t.Errorf("imported log sessionId = %q, want %q", got, session.ID)
			}
		}
	}
	if imported != 2 {
		t.Fatalf("imported log lines = %d, want 2", imported)
	}
}

// TestImportFailureLogCarriesSessionID verifies a per-file FAILURE log also
// carries the sessionId (verification failure path).
func TestImportFailureLogCarriesSessionID(t *testing.T) {
	h := newHarness(t)
	recs := h.withCapturingLogger(t)
	h.pipe.afterCopyHook = func(partialPath string) {
		_ = os.WriteFile(partialPath, []byte("corrupted-different!!"), 0o644)
	}

	h.writeFile("corruptme.jpg", "original-good-content", testDate)

	session, err := h.pipe.Run(context.Background(), h.copyOpts(), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if session.Failures != 1 {
		t.Fatalf("Failures = %d, want 1", session.Failures)
	}

	found := false
	for _, r := range *recs {
		if r.Message == "import file failed" {
			found = true
			if got := attrValue(r, "sessionId"); got != session.ID {
				t.Errorf("failure log sessionId = %q, want %q", got, session.ID)
			}
		}
	}
	if !found {
		t.Fatal("no 'import file failed' log captured")
	}
}

// TestImportAlreadyImportedLogCarriesSessionID verifies the per-file
// already-archived line (logged on a first run when two source files share
// content in copy mode) carries the sessionId.
func TestImportAlreadyImportedLogCarriesSessionID(t *testing.T) {
	h := newHarness(t)
	recs := h.withCapturingLogger(t)

	h.writeFile("a.jpg", "alpha", testDate)
	h.writeFile("dup.jpg", "alpha", testDate) // same content, different name -> already imported

	session, err := h.pipe.Run(context.Background(), h.copyOpts(), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if session.AlreadyImported != 1 {
		t.Fatalf("AlreadyImported = %d, want 1", session.AlreadyImported)
	}

	found := false
	for _, r := range *recs {
		if r.Message == "already archived, skipped" {
			found = true
			if got := attrValue(r, "sessionId"); got != session.ID {
				t.Errorf("already-imported log sessionId = %q, want %q", got, session.ID)
			}
		}
	}
	if !found {
		t.Fatal("no 'already archived, skipped' log captured")
	}
}

// TestImportSkipLogCarriesSessionID verifies the already-imported skip line
// carries the (new) sessionId when the same tree is re-run.
func TestImportSkipLogCarriesSessionID(t *testing.T) {
	h := newHarness(t)

	h.writeFile("a.jpg", "alpha", testDate)
	if _, err := h.pipe.Run(context.Background(), h.copyOpts(), nil); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Second run with a capturing logger: a.jpg is already-imported (skip).
	recs := h.withCapturingLogger(t)
	session, err := h.pipe.Run(context.Background(), h.copyOpts(), nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	found := false
	for _, r := range *recs {
		if r.Message == "already archived, skipped" {
			found = true
			if !hasAttr(r, "sessionId") {
				t.Errorf("skip log missing sessionId")
			}
			if got := attrValue(r, "sessionId"); got != session.ID {
				t.Errorf("skip log sessionId = %q, want %q", got, session.ID)
			}
		}
	}
	if !found {
		t.Fatal("expected an 'already archived, skipped' log on the second run")
	}
}
