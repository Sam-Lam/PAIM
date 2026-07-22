package importer

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Sam-Lam/PAIM/internal/archive"
	"github.com/Sam-Lam/PAIM/internal/mediatype"
)

// DryRunReport is the non-mutating prediction of an import. It also carries the
// per-file quick (and any computed full) hashes forward so a subsequent import
// need not recompute them.
//
// Duplicates counts both files whose content already exists in the asset DB AND
// intra-batch duplicates: content that appears two or more times within the very
// same scan. The DB cannot see the batch itself, so the first occurrence of a
// repeated content is predicted New (it imports) and every later occurrence is
// predicted Duplicate — exactly what the import records — making the dry run an
// exact predictor of the subsequent session's New/Duplicates counters.
type DryRunReport struct {
	Files            int
	Photos           int // photo + raw_photo files
	Videos           int
	AlreadyImported  int
	Duplicates       int
	New              int
	TotalImportBytes int64
	EstimatedSeconds float64

	// Adopt-mode planning.
	PlannedAdoptions int // files that will be registered in place
	PlannedMoves     int // adopt+reorganize: files whose path will change

	// Carried-forward per-path data (keyed by absolute path).
	QuickHashes  map[string]string
	FullHashes   map[string]string
	Dispositions map[string]Disposition
	// Scans records the size and mtime each path had at dry-run time. A subsequent
	// import reuses QuickHashes/FullHashes for a path ONLY when the file still has
	// this exact size and mtime, so a file edited between analyze and import is
	// re-hashed instead of carrying a stale hash into verification.
	Scans map[string]ScanMeta
}

// ScanMeta is the size and mtime a file had when the dry run hashed it. It is the
// staleness gate for reusing that file's precomputed hashes at import time.
type ScanMeta struct {
	Size    int64
	ModTime int64 // unix seconds, matching FileInfo.ModTime
}

// throughputFloor is the assumed lower bound on disk throughput (100 MiB/s) used
// when the measured hashing throughput is unavailable or implausibly small.
const throughputFloor = 100 << 20

// DryRun computes quick hashes for every scanned file (using a bounded worker
// pool) and classifies each against the asset DB, producing counts and an
// estimated duration WITHOUT modifying anything (no file writes; the only DB
// write is the harmless FullHash backfill on an existing asset when a quick-hash
// collision forces full hashing). The returned report's hashes are reusable by
// Run via Options.Precomputed.
//
// EstimatedSeconds heuristic: the wall-clock throughput of the quick-hash phase
// (bytes actually read / elapsed) approximates raw disk read speed. A COPY
// import moves each new byte three times over the disk (read source, write dest,
// re-read for verification), so seconds ≈ 3 * TotalImportBytes / throughput. An
// ADOPT import performs no copy but reads every byte once for the BLAKE3 baseline
// (and again after a reorganize move), so seconds ≈ TotalImportBytes /
// throughput. A conservative 100 MiB/s floor guards against a tiny sample.
func (p *Pipeline) DryRun(ctx context.Context, scan *ScanResult, opts Options, progressFn ProgressFunc) (*DryRunReport, error) {
	report := &DryRunReport{
		QuickHashes:  make(map[string]string, len(scan.Files)),
		FullHashes:   make(map[string]string, len(scan.Files)),
		Dispositions: make(map[string]Disposition, len(scan.Files)),
		Scans:        make(map[string]ScanMeta, len(scan.Files)),
	}

	quick, hashedBytes, elapsed, err := p.hashAll(ctx, scan.Files, opts.concurrency(), scan.TotalBytes, progressFn)
	if err != nil {
		return nil, err
	}
	for path, h := range quick {
		report.QuickHashes[path] = h
	}

	throughput := float64(throughputFloor)
	if elapsed > 0 && hashedBytes > 0 {
		if measured := float64(hashedBytes) / elapsed.Seconds(); measured > throughput {
			throughput = measured
		}
	}

	lay := p.effectiveLayout(opts.DestinationRoot)

	for i, fi := range scan.Files {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("dry run: %w", err)
		}
		report.Files++
		// Record the analyze-time size+mtime so a later import can gate hash reuse.
		report.Scans[fi.Path] = ScanMeta{Size: fi.Size, ModTime: fi.ModTime}
		switch fi.Kind {
		case mediatype.Photo, mediatype.RawPhoto:
			report.Photos++
		case mediatype.Video:
			report.Videos++
		}

		cls, err := p.classify(ctx, fi.Path, quick[fi.Path], "")
		if err != nil {
			// A read failure during dry run is non-fatal to the prediction; treat
			// the file as new and note it.
			p.log.Warn("dry run: classify failed", "path", fi.Path, "error", err.Error())
			cls = classification{Disposition: DispositionNew, QuickHash: quick[fi.Path]}
		}
		report.Dispositions[fi.Path] = cls.Disposition
		if cls.FullHash != "" {
			report.FullHashes[fi.Path] = cls.FullHash
		}

		switch cls.Disposition {
		case DispositionAlreadyImported:
			report.AlreadyImported++
		case DispositionDuplicate:
			report.Duplicates++
		case DispositionNew:
			report.New++
			report.TotalImportBytes += fi.Size
			if opts.mode() == ModeAdopt {
				report.PlannedAdoptions++
				if opts.Reorganize {
					// Prediction only: approximate the capture date with the file
					// mtime (real capture date needs a metadata pass, done at import
					// time). This can occasionally mis-predict a move across a
					// midnight boundary but keeps the dry run cheap.
					dest := computeDestination(lay, time.Unix(fi.ModTime, 0), opts.EventName, fi)
					if dest != fi.Path {
						report.PlannedMoves++
					}
				}
			}
		}
		progressFn.emit(Progress{
			Phase:       PhaseClassifying,
			FilesDone:   i + 1,
			FilesTotal:  len(scan.Files),
			BytesDone:   scan.TotalBytes,
			BytesTotal:  scan.TotalBytes,
			CurrentFile: fi.Path,
		})
	}

	// The DB-driven classification above cannot recognize duplicates that live
	// entirely within this batch (the assets do not exist yet). Predict them the
	// way the import will actually record them: the first occurrence of a repeated
	// content imports, later occurrences become duplicate rows.
	p.predictIntraBatchDuplicates(ctx, scan, opts, lay, quick, report)

	multiplier := 3.0
	if opts.mode() == ModeAdopt {
		multiplier = 1.0
	}
	report.EstimatedSeconds = multiplier * float64(report.TotalImportBytes) / throughput

	p.log.Info("dry run complete",
		"files", report.Files, "new", report.New, "duplicates", report.Duplicates,
		"alreadyImported", report.AlreadyImported, "importBytes", report.TotalImportBytes,
		"estimatedSeconds", report.EstimatedSeconds)
	return report, nil
}

// predictIntraBatchDuplicates reclassifies later occurrences of content repeated
// within the scanned batch from New to Duplicate. It runs AFTER the DB-driven
// classification and only touches files the DB left as New (a DB match already
// carries the authoritative disposition). Files are grouped by quick hash and
// each colliding group is confirmed with full hashes — a quick-hash collision
// alone is never a duplicate, mirroring the two-stage rule of the DB path. The
// first occurrence in scan order stays New (it imports); every subsequent
// full-hash match becomes a Duplicate, with the counters and byte/adopt tallies
// adjusted to match what the import will record.
func (p *Pipeline) predictIntraBatchDuplicates(ctx context.Context, scan *ScanResult, opts Options, lay *archive.Layout, quick map[string]string, report *DryRunReport) {
	// Group the New-classified files by quick hash, preserving scan order so the
	// "first occurrence imports" rule is deterministic.
	byQuick := make(map[string][]FileInfo)
	for _, fi := range scan.Files {
		if report.Dispositions[fi.Path] != DispositionNew {
			continue
		}
		qh := quick[fi.Path]
		if qh == "" {
			continue // hash unavailable; cannot confirm identity
		}
		byQuick[qh] = append(byQuick[qh], fi)
	}

	for _, group := range byQuick {
		if len(group) < 2 {
			continue // no intra-batch quick-hash collision
		}
		firstByFull := make(map[string]string, len(group)) // full hash -> first path
		for _, fi := range group {
			if err := ctx.Err(); err != nil {
				return
			}
			fh := report.FullHashes[fi.Path]
			if fh == "" {
				computed, err := p.fullHash(ctx, fi.Path)
				if err != nil {
					p.log.Warn("dry run: intra-batch full hash failed", "path", fi.Path, "error", err.Error())
					continue
				}
				fh = computed
				report.FullHashes[fi.Path] = fh
			}
			if _, seen := firstByFull[fh]; !seen {
				firstByFull[fh] = fi.Path
				continue // first occurrence of this content: it imports, stays New
			}
			// A later occurrence of identical content: the import records it as a
			// duplicate row (not copied/adopted anew).
			report.Dispositions[fi.Path] = DispositionDuplicate
			report.New--
			report.Duplicates++
			report.TotalImportBytes -= fi.Size
			if opts.mode() == ModeAdopt {
				report.PlannedAdoptions--
				if opts.Reorganize {
					dest := computeDestination(lay, time.Unix(fi.ModTime, 0), opts.EventName, fi)
					if dest != fi.Path {
						report.PlannedMoves--
					}
				}
			}
		}
	}
}

// hashAll computes the quick hash of every file using a bounded worker pool. It
// returns the map of path->quick hash, the total bytes read (for the throughput
// estimate), and the elapsed wall time. A per-file hash failure is logged and
// omitted from the map (the caller treats a missing hash as needing recompute).
func (p *Pipeline) hashAll(ctx context.Context, files []FileInfo, concurrency int, totalBytes int64, progressFn ProgressFunc) (map[string]string, int64, time.Duration, error) {
	if concurrency < 1 {
		concurrency = 1
	}
	type job struct {
		idx int
		fi  FileInfo
	}
	type result struct {
		path string
		hash string
		read int64
		size int64
		err  error
	}

	jobs := make(chan job)
	results := make(chan result)
	var wg sync.WaitGroup

	start := time.Now()
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if err := ctx.Err(); err != nil {
					results <- result{path: j.fi.Path, err: err}
					continue
				}
				h, err := p.quickHash(j.fi.Path)
				// Bytes actually read by the quick hash: whole file for small
				// files, else two 4 MiB chunks.
				read := j.fi.Size
				if read > (8 << 20) {
					read = 8 << 20
				}
				results <- result{path: j.fi.Path, hash: h, read: read, size: j.fi.Size, err: err}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for i, fi := range files {
			select {
			case <-ctx.Done():
				return
			case jobs <- job{idx: i, fi: fi}:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	out := make(map[string]string, len(files))
	var totalRead int64
	var bytesDone int64
	done := 0
	var firstErr error
	for r := range results {
		done++
		if r.err != nil {
			if firstErr == nil && ctx.Err() != nil {
				firstErr = r.err
			}
			if ctx.Err() == nil {
				p.log.Warn("hash: quick hash failed", "path", r.path, "error", r.err.Error())
			}
			continue
		}
		out[r.path] = r.hash
		totalRead += r.read
		bytesDone += r.size
		if done%32 == 0 {
			progressFn.emit(Progress{
				Phase:       PhaseHashing,
				FilesDone:   done,
				FilesTotal:  len(files),
				BytesDone:   bytesDone,
				BytesTotal:  totalBytes,
				CurrentFile: r.path,
			})
		}
	}
	// Final hashing snapshot so the counters land on 100% of the hashing phase
	// even when the file count is not a multiple of the sampling stride.
	progressFn.emit(Progress{
		Phase:      PhaseHashing,
		FilesDone:  done,
		FilesTotal: len(files),
		BytesDone:  bytesDone,
		BytesTotal: totalBytes,
	})
	if firstErr != nil {
		return nil, 0, 0, fmt.Errorf("hash all: %w", firstErr)
	}
	return out, totalRead, time.Since(start), nil
}
