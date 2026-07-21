package importer

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Sam-Lam/PAIM/internal/hashing"
	"github.com/Sam-Lam/PAIM/internal/mediatype"
)

// DryRunReport is the non-mutating prediction of an import. It also carries the
// per-file quick (and any computed full) hashes forward so a subsequent import
// need not recompute them.
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
	}

	quick, hashedBytes, elapsed, err := p.hashAll(ctx, scan.Files, opts.concurrency(), progressFn)
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
		switch fi.Kind {
		case mediatype.Photo, mediatype.RawPhoto:
			report.Photos++
		case mediatype.Video:
			report.Videos++
		}

		cls, err := p.classify(ctx, fi.Path, quick[fi.Path])
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
			Phase:      PhaseHashing,
			FilesDone:  i + 1,
			FilesTotal: len(scan.Files),
			BytesDone:  report.TotalImportBytes,
			BytesTotal: scan.TotalBytes,
		})
	}

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

// hashAll computes the quick hash of every file using a bounded worker pool. It
// returns the map of path->quick hash, the total bytes read (for the throughput
// estimate), and the elapsed wall time. A per-file hash failure is logged and
// omitted from the map (the caller treats a missing hash as needing recompute).
func (p *Pipeline) hashAll(ctx context.Context, files []FileInfo, concurrency int, progressFn ProgressFunc) (map[string]string, int64, time.Duration, error) {
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
				h, err := hashing.QuickHash(j.fi.Path)
				// Bytes actually read by the quick hash: whole file for small
				// files, else two 4 MiB chunks.
				read := j.fi.Size
				if read > (8 << 20) {
					read = 8 << 20
				}
				results <- result{path: j.fi.Path, hash: h, read: read, err: err}
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
		if done%32 == 0 {
			progressFn.emit(Progress{Phase: PhaseHashing, FilesDone: done, FilesTotal: len(files), CurrentFile: r.path})
		}
	}
	if firstErr != nil {
		return nil, 0, 0, fmt.Errorf("hash all: %w", firstErr)
	}
	return out, totalRead, time.Since(start), nil
}
