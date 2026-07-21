package services

import (
	"context"
	"log/slog"

	"github.com/autolinepro/paim/internal/hashing"
	"github.com/autolinepro/paim/internal/mediatype"
	"github.com/autolinepro/paim/internal/repo"
	"github.com/autolinepro/paim/internal/source"
	"github.com/autolinepro/paim/internal/volumes"
)

// Hasher adapts internal/hashing to the source package's FileHasher and
// FullHasher interfaces (QuickHash(path) / FullHash(path)). source.Identifier is
// constructed with one of these in main.go; the FullHash method (with an internal
// background context) lets safe-to-erase disambiguate quick-hash collisions.
type Hasher struct{}

// QuickHash returns the BLAKE3 quick hash of path.
func (Hasher) QuickHash(path string) (string, error) { return hashing.QuickHash(path) }

// FullHash returns the BLAKE3 full hash of path. The source package's FullHasher
// interface takes no context, so a background context is used internally.
func (Hasher) FullHash(path string) (string, error) {
	return hashing.FullHash(context.Background(), path)
}

// assetLookupAdapter adapts *repo.AssetRepo to source.AssetLookup, projecting
// domain assets into the minimal source.ArchivedAsset view (with backup
// completeness folded in).
type assetLookupAdapter struct {
	assets *repo.AssetRepo
}

// FindByQuickHash returns archived-asset views for every non-deleted asset with
// the given quick hash.
func (a assetLookupAdapter) FindByQuickHash(ctx context.Context, quickHash string) ([]source.ArchivedAsset, error) {
	rows, err := a.assets.FindByQuickHash(ctx, quickHash)
	if err != nil {
		return nil, err
	}
	out := make([]source.ArchivedAsset, 0, len(rows))
	for _, r := range rows {
		out = append(out, source.ArchivedAsset{
			ID:             r.ID,
			QuickHash:      r.QuickHash,
			FullHash:       r.FullHash,
			Verified:       r.VerificationStatus == domainVerified,
			BackupComplete: r.BackupStatus == domainBackupComplete,
		})
	}
	return out, nil
}

// SourcesService lists volumes, identifies import sources, and evaluates
// safe-to-erase.
type SourcesService struct {
	gated
	collector  *volumes.Collector
	identifier *source.Identifier
	sources    *repo.SourceRepo
	assets     *repo.AssetRepo
	watcher    *volumes.Watcher
	emitter    Emitter
	log        *slog.Logger
}

// Bind wires the SourcesService to an open library's identifier and repos in
// place.
func (s *SourcesService) Bind(core *AppCore) {
	s.identifier = core.Identifier
	s.sources = core.Sources
	s.assets = core.Assets
}

// NewSourcesService constructs a SourcesService.
func NewSourcesService(collector *volumes.Collector, identifier *source.Identifier, sources *repo.SourceRepo, assets *repo.AssetRepo, watcher *volumes.Watcher, emitter Emitter, logger *slog.Logger) *SourcesService {
	if logger == nil {
		logger = slog.Default()
	}
	return &SourcesService{
		collector:  collector,
		identifier: identifier,
		sources:    sources,
		assets:     assets,
		watcher:    watcher,
		emitter:    emitter,
		log:        logger.With(slog.String("subsystem", "source")),
	}
}

// ListVolumes enumerates and describes every mounted volume under /Volumes.
func (s *SourcesService) ListVolumes(ctx context.Context) ([]VolumeDTO, error) {
	infos, err := s.collector.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]VolumeDTO, 0, len(infos))
	for _, v := range infos {
		out = append(out, toVolumeDTO(v))
	}
	return out, nil
}

// MatchDTO is the JSON-friendly result of identifying a volume.
type MatchDTO struct {
	SourceID                   string    `json:"sourceId"`
	Source                     SourceDTO `json:"source"`
	Confidence                 int       `json:"confidence"`
	Reasons                    []string  `json:"reasons"`
	IsKnown                    bool      `json:"isKnown"`
	ContentsPreviouslyImported bool      `json:"contentsPreviouslyImported"`
}

// IdentifyVolume identifies the volume at mountPoint, persists the resulting
// source (creating a new record or updating the matched one and its LastSeen),
// emits source:identified, and returns the match with confidence and reasons.
func (s *SourcesService) IdentifyVolume(ctx context.Context, mountPoint string) (MatchDTO, error) {
	if err := s.guard(); err != nil {
		return MatchDTO{}, err
	}
	match, err := s.identifier.Identify(ctx, mountPoint)
	if err != nil {
		return MatchDTO{}, err
	}

	rec := match.SourceRecord
	now := timeNow()
	if match.IsKnown && rec.ID != "" {
		rec.LastSeenAt = now
		if err := s.sources.Update(ctx, rec); err != nil {
			return MatchDTO{}, err
		}
	} else {
		rec.LastSeenAt = now
		if err := s.sources.Create(ctx, rec); err != nil {
			return MatchDTO{}, err
		}
	}

	emitSafe(s.emitter, EventSourceIdentified, SourceIdentified{
		MountPoint: mountPoint,
		SourceID:   rec.ID,
		Confidence: match.Confidence,
		IsKnown:    match.IsKnown,
	})

	return MatchDTO{
		SourceID:                   rec.ID,
		Source:                     toSourceDTO(*rec),
		Confidence:                 match.Confidence,
		Reasons:                    match.Reasons,
		IsKnown:                    match.IsKnown,
		ContentsPreviouslyImported: match.ContentsPreviouslyImported,
	}, nil
}

// ListKnownSources returns the most recently seen persisted sources.
func (s *SourcesService) ListKnownSources(ctx context.Context) ([]SourceDTO, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.sources.ListRecent(ctx, 200)
	if err != nil {
		return nil, err
	}
	out := make([]SourceDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, toSourceDTO(r))
	}
	return out, nil
}

// SafeToEraseDTO is the JSON-friendly safe-to-erase report.
type SafeToEraseDTO struct {
	SourceID         string `json:"sourceId"`
	Safe             bool   `json:"safe"`
	Reason           string `json:"reason"`
	TotalMedia       int    `json:"totalMedia"`
	Archived         int    `json:"archived"`
	New              int    `json:"new"`
	Unverified       int    `json:"unverified"`
	BackupIncomplete int    `json:"backupIncomplete"`
}

// EvaluateSafeToErase walks the volume at mountPoint, decides whether it is safe
// to erase, persists the conclusion on the source (when sourceID is set), and
// returns the report.
func (s *SourcesService) EvaluateSafeToErase(ctx context.Context, sourceID, mountPoint string) (SafeToEraseDTO, error) {
	if err := s.guard(); err != nil {
		return SafeToEraseDTO{}, err
	}
	report, err := s.identifier.EvaluateSafeToErase(ctx, sourceID, mountPoint, assetLookupAdapter{assets: s.assets}, mediatype.IsMedia)
	if err != nil {
		return SafeToEraseDTO{}, err
	}
	if sourceID != "" {
		if err := s.sources.SetSafeToErase(ctx, sourceID, report.Safe, report.Reason); err != nil {
			s.log.Warn("could not persist safe-to-erase", "sourceId", sourceID, "error", err.Error())
		}
	}
	return SafeToEraseDTO{
		SourceID:         report.SourceID,
		Safe:             report.Safe,
		Reason:           report.Reason,
		TotalMedia:       report.TotalMedia,
		Archived:         report.Archived,
		New:              report.New,
		Unverified:       report.Unverified,
		BackupIncomplete: report.BackupIncomplete,
	}, nil
}

// StartWatching runs the volume watcher until ctx is cancelled, emitting
// volume:mounted / volume:unmounted for each change. It is invoked once from
// main.go in a background goroutine; the watcher establishes its baseline from
// the volumes already mounted at start (which produce no events).
func (s *SourcesService) StartWatching(ctx context.Context) error {
	if s.watcher == nil {
		return nil
	}
	events, err := s.watcher.Start(ctx)
	if err != nil {
		return err
	}
	go func() {
		for ev := range events {
			switch ev.Type {
			case volumes.EventMounted:
				s.log.Info("volume mounted", "mountPoint", ev.MountPoint)
				emitSafe(s.emitter, EventVolumeMounted, VolumeEvent{MountPoint: ev.MountPoint})
			case volumes.EventUnmounted:
				s.log.Info("volume unmounted", "mountPoint", ev.MountPoint)
				emitSafe(s.emitter, EventVolumeUnmounted, VolumeEvent{MountPoint: ev.MountPoint})
			}
		}
	}()
	return nil
}
