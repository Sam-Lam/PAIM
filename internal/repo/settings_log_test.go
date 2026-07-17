package repo

import (
	"context"
	"testing"
	"time"

	"github.com/autolinepro/paim/internal/domain"
)

func TestSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	r := NewSettingsRepo(newTestDB(t))

	// Absent keys return defaults.
	if v, err := r.GetString(ctx, "missing", "def"); err != nil || v != "def" {
		t.Fatalf("GetString(missing) = %q, %v; want def", v, err)
	}
	if v, err := r.GetInt(ctx, "missing", 7); err != nil || v != 7 {
		t.Fatalf("GetInt(missing) = %d, %v; want 7", v, err)
	}
	if v, err := r.GetBool(ctx, "missing", true); err != nil || v != true {
		t.Fatalf("GetBool(missing) = %v, %v; want true", v, err)
	}

	if err := r.Set(ctx, "workers", 4); err != nil {
		t.Fatalf("set int: %v", err)
	}
	if err := r.Set(ctx, "destRoot", "/Photos"); err != nil {
		t.Fatalf("set string: %v", err)
	}
	if err := r.Set(ctx, "darkMode", true); err != nil {
		t.Fatalf("set bool: %v", err)
	}

	if v, err := r.GetInt(ctx, "workers", 2); err != nil || v != 4 {
		t.Errorf("GetInt(workers) = %d, %v; want 4", v, err)
	}
	if v, err := r.GetString(ctx, "destRoot", ""); err != nil || v != "/Photos" {
		t.Errorf("GetString(destRoot) = %q, %v; want /Photos", v, err)
	}
	if v, err := r.GetBool(ctx, "darkMode", false); err != nil || v != true {
		t.Errorf("GetBool(darkMode) = %v, %v; want true", v, err)
	}

	// Set again overwrites (upsert on conflict).
	if err := r.Set(ctx, "workers", 8); err != nil {
		t.Fatalf("update int: %v", err)
	}
	if v, _ := r.GetInt(ctx, "workers", 0); v != 8 {
		t.Errorf("GetInt(workers) after update = %d, want 8", v)
	}

	// Struct round-trip via Get/Set.
	type layout struct {
		Pattern string `json:"pattern"`
		RAWSub  bool   `json:"rawSub"`
	}
	if err := r.Set(ctx, "layout", layout{Pattern: "YYYY/MM", RAWSub: true}); err != nil {
		t.Fatalf("set struct: %v", err)
	}
	var got layout
	found, err := r.Get(ctx, "layout", &got)
	if err != nil || !found {
		t.Fatalf("Get(layout) found=%v err=%v", found, err)
	}
	if got.Pattern != "YYYY/MM" || !got.RAWSub {
		t.Errorf("layout round-trip = %+v", got)
	}
}

func TestLogBatchInsertAndSearch(t *testing.T) {
	ctx := context.Background()
	r := NewLogRepo(newTestDB(t))

	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	entries := []domain.LogEntry{
		{Timestamp: base, Level: domain.LogLevelInfo, Subsystem: "importer", Message: "started import"},
		{Timestamp: base.Add(time.Minute), Level: domain.LogLevelError, Subsystem: "backup", Message: "upload failed"},
		{Timestamp: base.Add(2 * time.Minute), Level: domain.LogLevelInfo, Subsystem: "importer", Message: "copied file"},
	}
	if err := r.BatchInsert(ctx, entries); err != nil {
		t.Fatalf("batch insert: %v", err)
	}

	// Empty batch is a no-op.
	if err := r.BatchInsert(ctx, nil); err != nil {
		t.Fatalf("empty batch insert: %v", err)
	}

	all, total, err := r.Search(ctx, LogQuery{})
	if err != nil {
		t.Fatalf("search all: %v", err)
	}
	if total != 3 || len(all) != 3 {
		t.Fatalf("search all: total %d len %d, want 3", total, len(all))
	}
	// Newest first.
	if all[0].Message != "copied file" {
		t.Errorf("ordering: first = %q, want newest 'copied file'", all[0].Message)
	}

	// Filter by subsystem.
	imp, total, err := r.Search(ctx, LogQuery{Subsystem: "importer"})
	if err != nil {
		t.Fatalf("search importer: %v", err)
	}
	if total != 2 || len(imp) != 2 {
		t.Errorf("importer search total %d len %d, want 2", total, len(imp))
	}

	// Filter by level + text.
	errs, total, err := r.Search(ctx, LogQuery{Level: domain.LogLevelError, Text: "failed"})
	if err != nil {
		t.Fatalf("search errors: %v", err)
	}
	if total != 1 || len(errs) != 1 || errs[0].Subsystem != "backup" {
		t.Errorf("error search wrong: total %d %+v", total, errs)
	}

	// Time range.
	ranged, total, err := r.Search(ctx, LogQuery{Since: base.Add(90 * time.Second)})
	if err != nil {
		t.Fatalf("search ranged: %v", err)
	}
	if total != 1 || ranged[0].Message != "copied file" {
		t.Errorf("time-range search wrong: total %d %+v", total, ranged)
	}

	// Export order is ascending.
	exp, err := r.ListForExport(ctx, LogQuery{})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(exp) != 3 || exp[0].Message != "started import" {
		t.Errorf("export order wrong: %+v", exp)
	}
}
