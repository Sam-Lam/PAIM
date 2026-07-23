package archive

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sam-Lam/PAIM/internal/mediatype"
)

// DestinationResolver computes destination paths like Layout.DestinationFor but
// adds the "sticky date folder" rule for EMPTY event names: within a single run
// it consults the year folder on disk once per (year, date) and, when EXACTLY
// ONE existing directory matches "YYYY-MM-DD*", routes new files into that
// folder (the user's labeled folder) instead of a bare "YYYY-MM-DD" sibling.
// Zero or 2+ matches fall back to the bare date folder. A non-empty event always
// targets "YYYY-MM-DD Event" exactly (no disk lookup). The bare "YYYY-MM-DD"
// folder itself counts as a match, so a date that only has its bare folder joins
// it — which is the historical behavior.
//
// Resolutions are cached for the life of the resolver so a run is deterministic
// even as files land in a folder mid-run. A resolver is single-run scoped and
// NOT safe for concurrent use; construct a fresh one per import/reorganize (the
// pipeline runs one operation at a time).
type DestinationResolver struct {
	layout *Layout
	// cache maps "yearFolder/dateFolder" -> resolved day-folder name.
	cache map[string]string
}

// NewDestinationResolver returns a resolver over lay with an empty cache.
func NewDestinationResolver(lay *Layout) *DestinationResolver {
	return &DestinationResolver{layout: lay, cache: make(map[string]string)}
}

// MasterRoot returns the underlying layout's master root.
func (r *DestinationResolver) MasterRoot() string { return r.layout.MasterRoot }

// DestinationFor mirrors Layout.DestinationFor but applies the sticky-date rule
// for empty events (see the type documentation). The returned path is relative
// to MasterRoot.
func (r *DestinationResolver) DestinationFor(captureDate time.Time, eventName, filename string, kind mediatype.Kind) string {
	yearFolder := captureDate.Format(r.layout.YearLayout)
	dateFolder := captureDate.Format(r.layout.DateLayout)

	dayFolder := dateFolder
	if event := sanitizeEvent(eventName); event != "" {
		// Explicit event: exact target, never sticky.
		dayFolder = dateFolder + " " + event
	} else {
		dayFolder = r.stickyDayFolder(yearFolder, dateFolder)
	}

	parts := []string{yearFolder, dayFolder}
	if kind == mediatype.RawPhoto {
		parts = append(parts, r.layout.RawSubfolder)
	}
	parts = append(parts, filename)
	return filepath.Join(parts...)
}

// stickyDayFolder resolves (and caches) the day-folder name for an empty-event
// destination under yearFolder for dateFolder.
func (r *DestinationResolver) stickyDayFolder(yearFolder, dateFolder string) string {
	key := yearFolder + "/" + dateFolder
	if cached, ok := r.cache[key]; ok {
		return cached
	}
	resolved := r.matchExistingDayFolder(yearFolder, dateFolder)
	r.cache[key] = resolved
	return resolved
}

// matchExistingDayFolder scans "<root>/<yearFolder>" for directories whose name
// is dateFolder or begins with "dateFolder " (a labeled sibling). Exactly one
// such directory wins; zero or multiple fall back to the bare dateFolder.
func (r *DestinationResolver) matchExistingDayFolder(yearFolder, dateFolder string) string {
	yearAbs := filepath.Join(r.layout.MasterRoot, yearFolder)
	entries, err := os.ReadDir(yearAbs)
	if err != nil {
		// Year dir missing/unreadable → nothing to stick to → bare date folder.
		return dateFolder
	}
	prefix := dateFolder + " "
	match := ""
	count := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == dateFolder || strings.HasPrefix(name, prefix) {
			count++
			if count > 1 {
				// 2+ matches → ambiguous → bare date folder.
				return dateFolder
			}
			match = name
		}
	}
	if count == 1 {
		return match
	}
	return dateFolder
}
