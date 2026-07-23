package archive

import (
	"regexp"
	"strings"
)

// dateFolderRe matches a folder name that begins with a "YYYY-MM-DD" date,
// optionally followed by a single space and a human label. Submatch 1 is the
// label part (empty for a pure date). It is anchored so only a genuine
// date-prefixed folder qualifies.
var dateFolderRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(?: (.*))?$`)

// yearFolderRe matches a bare 4-digit year folder (a library top-level folder).
var yearFolderRe = regexp.MustCompile(`^\d{4}$`)

// cameraDirRe matches generic camera-generated directory names such as
// "100CANON", "100_FUJI", "101MSDCF": three digits followed by 1..8 uppercase
// letters or underscores.
var cameraDirRe = regexp.MustCompile(`^\d{3}[A-Z_]{1,8}$`)

// excludedFolderNames are generic device/camera folders that never contribute a
// human label (compared case-insensitively).
var excludedFolderNames = map[string]bool{
	"DCIM":    true,
	"MISC":    true,
	"PRIVATE": true,
	"AVCHD":   true,
	"THMBNL":  true,
}

// DeriveLabel returns the event label to carry for a file whose current parent
// folder is named folderName, implementing the "labels survive reorganize" rule:
//
//   - A folder already in "YYYY-MM-DD Label" form contributes ONLY its Label.
//   - A pure "YYYY-MM-DD" date, a bare year "YYYY", the generic device folders
//     (DCIM, MISC, PRIVATE, AVCHD, THMBNL) and camera-roll folders matching
//     \d{3}[A-Z_]{1,8} (100CANON, 100_FUJI, …) contribute nothing.
//   - Anything else is passed through the event sanitizer.
//
// The returned label is already sanitized and safe as a single path component;
// an empty string means "no label" (the file lands in a bare date folder, unless
// the destination resolver's sticky rule routes it into an existing sibling).
func DeriveLabel(folderName string) string {
	name := strings.TrimSpace(folderName)
	if name == "" {
		return ""
	}
	// "YYYY-MM-DD" or "YYYY-MM-DD Label": keep only the label part (empty for a
	// pure date).
	if m := dateFolderRe.FindStringSubmatch(name); m != nil {
		return sanitizeEvent(m[1])
	}
	if yearFolderRe.MatchString(name) {
		return ""
	}
	if excludedFolderNames[strings.ToUpper(name)] {
		return ""
	}
	if cameraDirRe.MatchString(name) {
		return ""
	}
	return sanitizeEvent(name)
}
