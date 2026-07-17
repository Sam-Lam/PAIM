package cleanup

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/autolinepro/paim/internal/domain"
	"github.com/autolinepro/paim/internal/hashing"
	"gorm.io/gorm"
)

func containsSubstr(reasons []string, sub string) bool {
	for _, r := range reasons {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func realQuick(path string) (string, error) {
	return hashing.QuickHash(path)
}

// countRows sums the rows across the tables an analysis might otherwise touch, so
// a change reveals an accidental write.
func countRows(t *testing.T, gdb *gorm.DB) int64 {
	t.Helper()
	var total int64
	for _, model := range []any{&domain.Asset{}, &domain.BackupJob{}, &domain.BackupProvider{}} {
		var n int64
		if err := gdb.Model(model).Count(&n).Error; err != nil {
			t.Fatalf("count rows: %v", err)
		}
		total += n
	}
	return total
}
