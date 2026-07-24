package importer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sam-Lam/PAIM/internal/archive"
	"github.com/Sam-Lam/PAIM/internal/mediatype"
	"github.com/Sam-Lam/PAIM/internal/metadata"
)

// RetryOutcome reports the result of a single-file retry (see RetryFile).
// Exactly one of the two states holds: Failed=false (the file was re-processed
// through the real pipeline and is now recorded — as a verified asset, a flagged
// duplicate, or an already-imported skip; AssetID is set when a row was created),
// or Failed=true (it failed again, with FailOp/FailErr naming the stage and
// message).
type RetryOutcome struct {
	AssetID  string
	Resolved bool
	Failed   bool
	FailOp   string
	FailErr  string
}

// RetryFile re-attempts a SINGLE previously-failed file through the exact import
// pipeline machinery — hash → duplicate detection → (copy → fsync → BLAKE3
// verify → atomic rename →) record for copy mode, or the in-place baseline for
// adopt mode — honoring every hard rule; verification is never bypassed. It is
// driven by ImportService.RetryFailedFile under the one-active-operation guard,
// so no concurrent import runs while it reassigns the failure sink.
//
// Unlike a normal run, a re-failure here does NOT record a new structured
// failure row or bump the Failures counter: the sink is swapped for a capturing
// one so the caller can update the pre-existing failure record and keep counters
// coherent. Live Photo re-linking is intentionally out of scope for a per-file
// retry (a retried component records normally; pairing is a whole-session
// concern).
func (p *Pipeline) RetryFile(ctx context.Context, sessionID string, opts Options, path string) (RetryOutcome, error) {
	info, err := os.Stat(path)
	if err != nil {
		return RetryOutcome{}, fmt.Errorf("retry: source file %q: %w", path, err)
	}
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	fi := FileInfo{
		Path:    path,
		Size:    info.Size(),
		Ext:     ext,
		ModTime: info.ModTime().Unix(),
		Kind:    mediatype.KindOf(ext),
	}

	lay := p.effectiveLayout(opts.DestinationRoot)
	res := archive.NewDestinationResolver(lay)
	meta := p.extractOne(ctx, path)
	state := stateFromOptions(opts)

	var capturedOp, capturedErr string
	prev := p.failSink
	p.failSink = func(_, _, op string, wrapped error) {
		capturedOp = op
		capturedErr = wrapped.Error()
	}
	defer func() { p.failSink = prev }()

	var bytesDone int64
	outcome := p.processFile(ctx, sessionID, fi, opts, res, "", "", meta, &state, &bytesDone)
	if outcome.abort {
		return RetryOutcome{Failed: true, FailOp: "copy", FailErr: outcome.abortErr.Error()}, nil
	}
	if outcome.failed {
		return RetryOutcome{Failed: true, FailOp: capturedOp, FailErr: capturedErr}, nil
	}
	return RetryOutcome{AssetID: outcome.assetID, Resolved: true}, nil
}

// extractOne extracts metadata for a single path, degrading to nil (mtime
// fallback) when no extractor is configured or extraction fails.
func (p *Pipeline) extractOne(ctx context.Context, path string) *metadata.AssetMetadata {
	if p.extractor == nil {
		return nil
	}
	out, err := p.extractor.ExtractBatch(ctx, []string{path})
	if err != nil {
		p.log.Warn("retry: metadata extraction failed; proceeding degraded", "path", path, "error", err.Error())
		return nil
	}
	return out[path]
}
