package source

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Sam-Lam/PAIM/internal/domain"
	"github.com/Sam-Lam/PAIM/internal/volumes"
)

// Confidence scores (0..100) and the signals that earn them. Every score is
// paired with a human-readable reason recorded on the Match and ultimately in
// ImportSource.ConfidenceReason.
//
//	Signal                                   Score  Meaning
//	-------------------------------------    -----  ---------------------------------
//	Hardware serial match                    100    Same physical device (strongest).
//	Volume UUID match                        100    Same volume identity.
//	Content fingerprint (content hash) match 100    Same contents imported before.
//	Filesystem UUID + capacity + fs type     90     Same partition, unchanged geometry.
//	Filesystem UUID, capacity differs        60     Same UUID but possible reformat.
//	Filesystem UUID, fs type differs         55     Same UUID but reformatted filesystem.
//	Path fingerprint match (content differs) 70     Same layout, contents changed.
//	No match                                 0      New/unknown source.
const (
	scoreHardwareSerial   = 100
	scoreVolumeUUID       = 100
	scoreContentHash      = 100
	scoreFSFull           = 90
	scorePathOnly         = 70
	scoreFSCapacityDiffer = 60
	scoreFSTypeDiffer     = 55
	scoreNone             = 0
)

// VolumeDescriber describes a mounted volume. It is satisfied by
// *volumes.Collector and injected so the identifier is testable without shelling
// out to diskutil.
type VolumeDescriber interface {
	Describe(ctx context.Context, mountPoint string) (*volumes.Info, error)
}

// SourceStore is the subset of repo.SourceRepo the identifier needs to look up
// known sources by their strong identifiers.
type SourceStore interface {
	FindByHardwareSerial(ctx context.Context, serial string) (*domain.ImportSource, error)
	FindByVolumeUUID(ctx context.Context, volumeUUID string) (*domain.ImportSource, error)
	FindByFilesystemUUID(ctx context.Context, fsUUID string) (*domain.ImportSource, error)
	ListRecent(ctx context.Context, limit int) ([]domain.ImportSource, error)
}

// contentScanLimit bounds how many recent sources are compared by content
// fingerprint when no strong identifier matched.
const contentScanLimit = 200

// Match is the result of identifying the volume at a mount point.
type Match struct {
	// SourceRecord is the matched existing source (when IsKnown) or a new,
	// not-yet-persisted candidate describing this volume (when unknown).
	SourceRecord *domain.ImportSource
	// Confidence is the 0..100 identification confidence.
	Confidence int
	// Reasons explains every conclusion reached during identification.
	Reasons []string
	// IsKnown is true when the volume matched a previously-recorded source.
	IsKnown bool
	// ContentsPreviouslyImported is true when the volume's content fingerprint
	// matches a known source's stored fingerprint.
	ContentsPreviouslyImported bool
}

// Identifier identifies import sources and evaluates whether they are safe to
// erase. Construct it with NewIdentifier.
type Identifier struct {
	describer VolumeDescriber
	store     SourceStore
	hasher    FileHasher
	isMedia   func(ext string) bool
}

// NewIdentifier constructs an Identifier.
//
// The architecture lists the constructor dependencies as (volume describer,
// source store, file hasher); isMedia is passed here too because content
// fingerprinting needs the media-extension policy (from internal/mediatype) and
// keeping it on the identifier avoids threading it through every call.
func NewIdentifier(describer VolumeDescriber, store SourceStore, hasher FileHasher, isMedia func(ext string) bool) *Identifier {
	return &Identifier{describer: describer, store: store, hasher: hasher, isMedia: isMedia}
}

// Identify describes the volume at mountPoint, builds a candidate source record,
// computes its content fingerprint, and scores it against known sources. The
// fingerprint walk (the slow part) reports scanned-file progress via progressFn
// (which may be nil) and honours ctx cancellation between entries.
func (id *Identifier) Identify(ctx context.Context, mountPoint string, progressFn func(scanned int)) (*Match, error) {
	info, err := id.describer.Describe(ctx, mountPoint)
	if err != nil {
		return nil, fmt.Errorf("source: describe %q: %w", mountPoint, err)
	}

	candidate := candidateFromInfo(info)

	// Compute the content fingerprint (best-effort: a failure leaves the
	// fingerprint empty and is noted as a reason rather than aborting).
	var fp *Fingerprint
	var fpReason string
	if id.hasher != nil && id.isMedia != nil {
		f, ferr := ComputeFingerprint(ctx, mountPoint, id.hasher, id.isMedia, progressFn)
		if ferr != nil {
			fpReason = fmt.Sprintf("Content fingerprint unavailable: %v", ferr)
		} else {
			fp = f
			candidate.ContentFingerprint = f.JSON()
		}
	}

	m := &Match{SourceRecord: candidate, Confidence: scoreNone, IsKnown: false}
	for _, w := range info.Warnings {
		m.Reasons = append(m.Reasons, "Volume probe note: "+w)
	}
	if fpReason != "" {
		m.Reasons = append(m.Reasons, fpReason)
	}

	best := 0
	var bestSource *domain.ImportSource

	consider := func(s *domain.ImportSource, score int, reason string) {
		m.Reasons = append(m.Reasons, reason)
		if score > best {
			best = score
			bestSource = s
		}
	}

	// 1. Hardware serial — strongest identity.
	if info.HardwareSerial != "" {
		s, lerr := id.store.FindByHardwareSerial(ctx, info.HardwareSerial)
		if lerr != nil {
			return nil, fmt.Errorf("source: lookup by hardware serial: %w", lerr)
		}
		if s != nil {
			consider(s, scoreHardwareSerial, fmt.Sprintf(
				"Hardware serial %s matches known device %q last seen %s.",
				info.HardwareSerial, displayLabel(s), lastSeen(s)))
		}
	}

	// 2. Volume UUID.
	if info.VolumeUUID != "" {
		s, lerr := id.store.FindByVolumeUUID(ctx, info.VolumeUUID)
		if lerr != nil {
			return nil, fmt.Errorf("source: lookup by volume uuid: %w", lerr)
		}
		if s != nil {
			consider(s, scoreVolumeUUID, fmt.Sprintf(
				"Volume UUID %s matches known device %q.", info.VolumeUUID, displayLabel(s)))
		}
	}

	// 3. Filesystem UUID (+ capacity + fs type corroboration).
	if info.FilesystemUUID != "" {
		s, lerr := id.store.FindByFilesystemUUID(ctx, info.FilesystemUUID)
		if lerr != nil {
			return nil, fmt.Errorf("source: lookup by filesystem uuid: %w", lerr)
		}
		if s != nil {
			score, reason := scoreFilesystemMatch(s, info)
			consider(s, score, reason)
		}
	}

	// 4. Content fingerprint — recognises previously-imported contents even when
	// hardware identity is unavailable (e.g. a reformatted or cloned card).
	if fp != nil {
		if s, matched, reason := id.matchByContent(ctx, fp); reason != "" {
			if matched {
				m.ContentsPreviouslyImported = true
				consider(s, scoreContentHash, reason)
			} else {
				// Path-only match: same layout, different contents.
				consider(s, scorePathOnly, reason)
			}
		}
	}

	if bestSource != nil {
		m.SourceRecord = bestSource
		m.Confidence = best
		m.IsKnown = true
	} else {
		m.Confidence = scoreNone
		m.Reasons = append(m.Reasons, "No matching known device — treating as a new source.")
	}

	m.SourceRecord.Confidence = m.Confidence
	m.SourceRecord.ConfidenceReason = strings.Join(m.Reasons, " ")
	return m, nil
}

// matchByContent compares fp against the stored fingerprints of recent sources.
// It returns the best source, whether the match was a full content-hash match
// (as opposed to a weaker path-only match), and a reason. An empty reason means
// no source matched.
func (id *Identifier) matchByContent(ctx context.Context, fp *Fingerprint) (*domain.ImportSource, bool, string) {
	recent, err := id.store.ListRecent(ctx, contentScanLimit)
	if err != nil {
		return nil, false, ""
	}
	var pathOnly *domain.ImportSource
	for i := range recent {
		stored, perr := ParseFingerprint(recent[i].ContentFingerprint)
		if perr != nil || (stored.ContentHash == "" && stored.PathHash == "") {
			continue
		}
		if fp.ContentHash != "" && stored.ContentHash == fp.ContentHash {
			return &recent[i], true, fmt.Sprintf(
				"Content fingerprint matches a previous import of source %q (previously imported).",
				displayLabel(&recent[i]))
		}
		if pathOnly == nil && fp.PathHash != "" && stored.PathHash == fp.PathHash {
			pathOnly = &recent[i]
		}
	}
	if pathOnly != nil {
		return pathOnly, false, fmt.Sprintf(
			"File layout matches known source %q but contents differ — files were added or changed.",
			displayLabel(pathOnly))
	}
	return nil, false, ""
}

// scoreFilesystemMatch scores a filesystem-UUID match, corroborated (or not) by
// capacity and filesystem type. A reformatted volume can keep aspects of its
// identity while changing geometry, so partial matches score lower.
func scoreFilesystemMatch(s *domain.ImportSource, info *volumes.Info) (int, string) {
	capacityMatch := s.CapacityBytes == info.CapacityBytes
	fsTypeMatch := strings.EqualFold(s.FilesystemType, info.FilesystemType)

	switch {
	case capacityMatch && fsTypeMatch:
		return scoreFSFull, fmt.Sprintf(
			"Filesystem UUID %s, capacity, and filesystem type all match known source %q.",
			info.FilesystemUUID, displayLabel(s))
	case !capacityMatch:
		return scoreFSCapacityDiffer, fmt.Sprintf(
			"Filesystem UUID %s matches known source %q but capacity differs (%d vs %d) — possible reformat.",
			info.FilesystemUUID, displayLabel(s), info.CapacityBytes, s.CapacityBytes)
	default:
		return scoreFSTypeDiffer, fmt.Sprintf(
			"Filesystem UUID %s matches known source %q but filesystem type differs (%s vs %s) — possible reformat.",
			info.FilesystemUUID, displayLabel(s), info.FilesystemType, s.FilesystemType)
	}
}

// candidateFromInfo builds a not-yet-persisted ImportSource from a volume
// description, inferring the source type and copying every identity/display
// field. The volume label is stored for display but is never used for matching.
func candidateFromInfo(info *volumes.Info) *domain.ImportSource {
	return &domain.ImportSource{
		SourceType:     inferSourceType(info),
		HardwareSerial: info.HardwareSerial,
		FilesystemUUID: info.FilesystemUUID,
		FilesystemType: info.FilesystemType,
		VolumeUUID:     info.VolumeUUID,
		VolumeLabel:    info.VolumeName,
		Manufacturer:   info.Manufacturer,
		Model:          info.Model,
		CapacityBytes:  info.CapacityBytes,
		ConnectionType: string(info.ConnectionType),
		LastSeenAt:     time.Now(),
	}
}

// inferSourceType maps a volume's connection type and media heuristics to a
// domain.SourceType. Rules (documented so callers can predict classification):
//
//   - Network volume: smbfs -> smb_share, otherwise nas_folder.
//   - SDXC/card-reader connection -> sd_card.
//   - USB/Thunderbolt -> usb_ssd, unless the model/manufacturer looks like a
//     spinning disk (HDD keywords) -> external_hdd.
//   - Internal bus -> internal_folder.
//   - Unknown connection -> usb_ssd when removable, else internal_folder.
func inferSourceType(info *volumes.Info) domain.SourceType {
	if info.IsNetworkVolume || info.ConnectionType == volumes.ConnectionNetwork {
		if info.FilesystemType == "smbfs" {
			return domain.SourceTypeSMBShare
		}
		return domain.SourceTypeNASFolder
	}
	switch info.ConnectionType {
	case volumes.ConnectionSDXC:
		return domain.SourceTypeSDCard
	case volumes.ConnectionUSB, volumes.ConnectionThunderbolt:
		if looksLikeHDD(info) {
			return domain.SourceTypeExternalHDD
		}
		return domain.SourceTypeUSBSSD
	case volumes.ConnectionInternal:
		return domain.SourceTypeInternalFolder
	default:
		if info.Removable {
			return domain.SourceTypeUSBSSD
		}
		return domain.SourceTypeInternalFolder
	}
}

// hddKeywords are substrings that identify a rotational external drive by model
// or product-family name.
var hddKeywords = []string{
	"hdd", "hard drive", "hard disk", "rotational",
	"backup plus", "expansion", "my passport", "my book", "elements",
}

// looksLikeHDD reports whether a volume's model/manufacturer indicates a
// spinning disk. An explicit "ssd" marker always wins for solid-state.
func looksLikeHDD(info *volumes.Info) bool {
	s := strings.ToLower(info.Model + " " + info.Manufacturer)
	if strings.Contains(s, "ssd") {
		return false
	}
	for _, kw := range hddKeywords {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// displayLabel returns a human-friendly name for a source: its label if present,
// otherwise its model, manufacturer, or ID.
func displayLabel(s *domain.ImportSource) string {
	for _, v := range []string{s.VolumeLabel, s.Model, s.Manufacturer} {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	if s.ID != "" {
		return s.ID
	}
	return "unknown source"
}

// lastSeen formats a source's LastSeenAt for a reason string.
func lastSeen(s *domain.ImportSource) string {
	if s.LastSeenAt.IsZero() {
		return "never"
	}
	return s.LastSeenAt.Format("2006-01-02")
}
