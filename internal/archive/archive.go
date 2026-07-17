// Package archive resolves where an imported file is placed inside the master
// library. The default layout is
//
//	<MasterRoot>/YYYY/YYYY-MM-DD Event/[RAW/]<filename>
//
// with RAW photos routed into a RAW/ subfolder and photos and videos side by
// side. The event name is user-supplied per import session; an empty event
// yields a plain YYYY-MM-DD folder.
//
// Filename collisions at the destination are resolved by appending " (2)",
// " (3)", … before the extension.
package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/autolinepro/paim/internal/mediatype"
)

// Default format tokens for the layout. They are Go reference-time layouts:
// yearLayout formats the top-level year folder and dateLayout the per-day
// folder prefix, matching the spec's "YYYY/YYYY-MM-DD Event" template.
const (
	defaultYearLayout   = "2006"
	defaultDateLayout   = "2006-01-02"
	defaultRawSubfolder = "RAW"
)

// Layout computes destination paths for a single master library root. Its
// zero value is not usable; construct it with New. The format fields are
// exported so callers may override the default template from Settings.
type Layout struct {
	// MasterRoot is the absolute path to the root of the master library. It is
	// not embedded in the relative paths returned by DestinationFor.
	MasterRoot string

	// YearLayout and DateLayout are Go reference-time layouts for the year
	// folder and the date prefix of the event folder respectively.
	YearLayout string
	DateLayout string

	// RawSubfolder is the folder name (relative to the event folder) that RAW
	// photos are placed in.
	RawSubfolder string
}

// New returns a Layout for masterRoot using the default template
// (YYYY/YYYY-MM-DD Event, RAW files under RAW/).
func New(masterRoot string) *Layout {
	return &Layout{
		MasterRoot:   masterRoot,
		YearLayout:   defaultYearLayout,
		DateLayout:   defaultDateLayout,
		RawSubfolder: defaultRawSubfolder,
	}
}

// DestinationFor returns the path of filename relative to MasterRoot, using
// captureDate for the year and date folders and eventName for the event
// portion of the day folder. RAW photos (kind == mediatype.RawPhoto) are placed
// in the RAW subfolder; all other kinds sit directly in the event folder.
//
// eventName is sanitized (see the package documentation and sanitizeEvent):
// path separators and control characters supplied by the user are stripped so
// the event can never escape or nest below the intended day folder. An empty
// (or fully-sanitized-away) event yields a day folder of just YYYY-MM-DD.
func (l *Layout) DestinationFor(captureDate time.Time, eventName, filename string, kind mediatype.Kind) string {
	yearFolder := captureDate.Format(l.YearLayout)
	dayFolder := captureDate.Format(l.DateLayout)
	if event := sanitizeEvent(eventName); event != "" {
		dayFolder = dayFolder + " " + event
	}

	parts := []string{yearFolder, dayFolder}
	if kind == mediatype.RawPhoto {
		parts = append(parts, l.RawSubfolder)
	}
	parts = append(parts, filename)
	return filepath.Join(parts...)
}

// sanitizeEvent makes a user-supplied event name safe to use as a single path
// component. It removes path separators (so the event cannot introduce extra
// directory levels or escape the day folder) and control characters, then
// trims surrounding whitespace and dots (which are meaningless or unsafe as
// leading/trailing folder characters on macOS).
func sanitizeEvent(event string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':':
			return -1
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, event)
	return strings.Trim(cleaned, " .\t")
}

// ResolveCollision returns a filename that does not currently exist in absDir.
// If filename is free it is returned unchanged; otherwise " (2)", " (3)", … is
// appended before the extension until an unused name is found. Only final
// names are considered occupied — in-progress ".paim-partial-*" temp files are
// ignored. An error is returned only if the directory cannot be inspected.
func ResolveCollision(absDir, filename string) (string, error) {
	if free, err := isFree(absDir, filename); err != nil {
		return "", err
	} else if free {
		return filename, nil
	}

	ext := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, n, ext)
		free, err := isFree(absDir, candidate)
		if err != nil {
			return "", err
		}
		if free {
			return candidate, nil
		}
	}
}

// isFree reports whether name does not exist in absDir.
func isFree(absDir, name string) (bool, error) {
	_, err := os.Lstat(filepath.Join(absDir, name))
	if err == nil {
		return false, nil
	}
	if os.IsNotExist(err) {
		return true, nil
	}
	return false, fmt.Errorf("resolve collision: stat %q in %q: %w", name, absDir, err)
}
