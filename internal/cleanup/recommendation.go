package cleanup

import (
	"fmt"
	"strings"
)

// Recommendation is the delete-safety verdict derived from a Report per the Safe
// Delete Rules.
type Recommendation struct {
	// SafeToDelete is true only when every media file is already archived, all
	// matched assets are verified, their required backups are complete, and the
	// database is consistent.
	SafeToDelete bool `json:"safeToDelete"`
	// Title is a short label, e.g. "Already Archived" or "Partial Archive".
	Title string `json:"title"`
	// Summary is a one-line human-readable verdict matching the spec's examples.
	Summary string `json:"summary"`
	// Reasons lists, in priority order, why deletion is not recommended. Empty
	// when SafeToDelete is true.
	Reasons []string `json:"reasons"`
}

// Recommendation computes the delete-safety verdict for the report per the Safe
// Delete Rules. Deletion is recommended only when the folder contains at least
// one media file and every media file is already_archived with the matched
// assets verified, their required backups complete, and no DB inconsistencies.
// Non-media files are surfaced in the report but do not block.
func (r *Report) Recommendation() Recommendation {
	archived := r.Count(ClassAlreadyArchived)

	// Duplicates whose content is confirmed preserved in the archive (verified,
	// fully-backed original) are safe: the bytes survive deletion, so they count
	// toward coverage instead of blocking. Only duplicates NOT so preserved (e.g.
	// an in-folder copy of a still-new file) block.
	dupTotal := r.Count(ClassDuplicate)
	dupSafe := r.DuplicatesArchived
	if dupSafe > dupTotal {
		dupSafe = dupTotal
	}
	dupBlocking := dupTotal - dupSafe

	var reasons []string
	if r.MediaFiles == 0 {
		reasons = append(reasons, "no media files found to archive")
	}
	if n := r.Count(ClassNew); n > 0 {
		reasons = append(reasons, fmt.Sprintf("%s not yet archived", files(n)))
	}
	if dupBlocking > 0 {
		reasons = append(reasons, fmt.Sprintf("%s are duplicates (resolve in the Duplicate Manager first)", files(dupBlocking)))
	}
	if n := r.Count(ClassVerificationFailed); n > 0 {
		reasons = append(reasons, fmt.Sprintf("%s failed verification", files(n)))
	}
	if r.UnreadableMedia > 0 {
		reasons = append(reasons, fmt.Sprintf("%s could not be read", files(r.UnreadableMedia)))
	}
	if r.ArchivedNotVerified > 0 {
		reasons = append(reasons, fmt.Sprintf("%s archived assets are not yet verified", count(r.ArchivedNotVerified)))
	}
	if r.BackupIncomplete > 0 {
		reasons = append(reasons, fmt.Sprintf("%s archived assets have incomplete backups", count(r.BackupIncomplete)))
	}
	if r.DBInconsistencies > 0 {
		reasons = append(reasons, fmt.Sprintf("%s archived assets are missing their archived copy on disk", count(r.DBInconsistencies)))
	}

	// Informational (non-blocking) note surfacing the safe duplicates.
	var notes []string
	if dupSafe > 0 {
		notes = append(notes, fmt.Sprintf("%s of archived assets — content preserved in archive", duplicates(dupSafe)))
	}

	// Coverage is satisfied when every media file is either already_archived or a
	// duplicate whose content is preserved in the archive.
	if len(reasons) == 0 && r.MediaFiles > 0 && archived+dupSafe == r.MediaFiles {
		summary := fmt.Sprintf("Already Archived — %s — %s — Safe to Delete",
			assets(archived), humanBytes(r.Bytes(ClassAlreadyArchived)))
		return Recommendation{SafeToDelete: true, Title: "Already Archived", Summary: summary, Reasons: notes}
	}

	// Defensive: if no specific blocker fired but not every media file is archived
	// (e.g. all unknown non-media), still refuse.
	if len(reasons) == 0 {
		reasons = append(reasons, "not every media file is archived")
	}

	title := "Partial Archive"
	if archived == 0 {
		title = "Not Archived"
	}
	return Recommendation{
		SafeToDelete: false,
		Title:        title,
		Summary:      title + " — Deletion NOT Recommended",
		Reasons:      append(reasons, notes...),
	}
}

// files renders a count with the word "file(s)".
func files(n int) string {
	if n == 1 {
		return "1 file"
	}
	return count(n) + " files"
}

// duplicates renders a count with the word "duplicate(s)".
func duplicates(n int) string {
	if n == 1 {
		return "1 duplicate"
	}
	return count(n) + " duplicates"
}

// assets renders a count with the word "asset(s)".
func assets(n int) string {
	if n == 1 {
		return "1 asset"
	}
	return count(n) + " assets"
}

// count renders an integer with thousands separators (e.g. 1,247).
func count(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
		if len(s) > lead {
			b.WriteByte(',')
		}
	}
	for i := lead; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteByte(',')
		}
	}
	out := b.String()
	if neg {
		return "-" + out
	}
	return out
}

// humanBytes renders a byte count as a human-readable size (e.g. 3.1 TB).
func humanBytes(b int64) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	value := float64(b) / float64(div)
	return fmt.Sprintf("%.1f %cB", value, "kMGTPE"[exp])
}
