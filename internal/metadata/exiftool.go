package metadata

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// batchChunk is the number of files sent per exiftool -execute during
// ExtractBatch. Batching amortizes the (small) per-request protocol overhead
// across many files for import throughput.
const batchChunk = 50

// ExifTool extracts metadata via a single persistent exiftool process launched
// with "-stay_open True -@ -". All access is serialized by a mutex (exiftool's
// stay_open protocol is strictly request/response over one pipe pair), and the
// process is killed and restarted once if a request fails or its output is
// malformed.
//
// Context handling: a cancelled ctx is detected before a request is sent and
// after the response is received. Additionally, if ctx is cancelled while
// waiting for exiftool's reply, the process is killed (so the wait returns) and
// restarted on the next call — the stay_open protocol has no in-band way to
// abort a request that is already in flight.
type ExifTool struct {
	path   string
	logger *slog.Logger

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *ringBuffer
}

var _ Extractor = (*ExifTool)(nil)

// newExifTool constructs an ExifTool bound to the given binary path. The process
// is started lazily on first use.
func newExifTool(path string, logger *slog.Logger) *ExifTool {
	return &ExifTool{path: path, logger: loggerOrDefault(logger)}
}

// Available reports that full-fidelity extraction is possible.
func (e *ExifTool) Available() bool { return true }

// Extract reads metadata for a single file.
func (e *ExifTool) Extract(ctx context.Context, path string) (*AssetMetadata, error) {
	out, err := e.run(ctx, buildArgs([]string{path}))
	if err != nil {
		return nil, fmt.Errorf("exiftool extract %q: %w", path, err)
	}
	metas, err := parseExifJSON(out)
	if err != nil {
		return nil, fmt.Errorf("exiftool extract %q: %w", path, err)
	}
	if len(metas) == 0 {
		return nil, fmt.Errorf("exiftool extract %q: no metadata returned", path)
	}
	return metas[0], nil
}

// ExtractBatch reads metadata for many files, chunking them across requests and
// keying the result by each record's SourceFile.
func (e *ExifTool) ExtractBatch(ctx context.Context, paths []string) (map[string]*AssetMetadata, error) {
	result := make(map[string]*AssetMetadata, len(paths))
	for start := 0; start < len(paths); start += batchChunk {
		end := start + batchChunk
		if end > len(paths) {
			end = len(paths)
		}
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("exiftool batch cancelled: %w", err)
		}
		chunk := paths[start:end]
		out, err := e.run(ctx, buildArgs(chunk))
		if err != nil {
			return result, fmt.Errorf("exiftool batch [%d:%d]: %w", start, end, err)
		}
		metas, err := parseExifJSON(out)
		if err != nil {
			return result, fmt.Errorf("exiftool batch [%d:%d]: %w", start, end, err)
		}
		for _, m := range metas {
			result[m.SourceFile] = m
		}
	}
	return result, nil
}

// run performs one mutex-guarded request, restarting the process and retrying
// once if the first attempt fails for any reason other than context
// cancellation.
func (e *ExifTool) run(ctx context.Context, args []string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	out, err := e.attempt(ctx, args)
	if err == nil {
		return out, nil
	}
	if ctx.Err() != nil {
		// Do not retry a cancelled request; propagate the cancellation.
		return nil, err
	}

	e.logger.Warn("exiftool request failed; restarting process and retrying once",
		"error", err)
	e.stop()

	out, err = e.attempt(ctx, args)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// attempt ensures the process is running, sends the request, and reads the
// response. It must be called with e.mu held.
func (e *ExifTool) attempt(ctx context.Context, args []string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := e.ensureStarted(); err != nil {
		return nil, err
	}
	if err := e.write(args); err != nil {
		return nil, err
	}
	out, err := e.read(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ensureStarted launches the stay_open process if it is not already running.
func (e *ExifTool) ensureStarted() error {
	if e.cmd != nil {
		return nil
	}
	cmd := exec.Command(e.path, "-stay_open", "True", "-@", "-")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("exiftool stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("exiftool stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("exiftool stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting exiftool %q: %w", e.path, err)
	}

	e.cmd = cmd
	e.stdin = stdin
	e.stdout = bufio.NewReader(stdout)
	e.stderr = newRingBuffer(64)
	// Drain stderr so exiftool never blocks on a full pipe; keep the tail for
	// error diagnostics.
	go func(rb *ringBuffer, r io.Reader) {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			rb.add(sc.Text())
		}
	}(e.stderr, stderrPipe)

	return nil
}

// write sends one request: each argument on its own line, terminated by
// -execute. Must be called with e.mu held and the process started.
func (e *ExifTool) write(args []string) error {
	var b strings.Builder
	for _, a := range args {
		b.WriteString(a)
		b.WriteByte('\n')
	}
	b.WriteString("-execute\n")
	if _, err := io.WriteString(e.stdin, b.String()); err != nil {
		return fmt.Errorf("writing to exiftool: %w", err)
	}
	return nil
}

// read collects stdout up to the {ready} marker. If ctx is cancelled while
// waiting, the process is killed so the read unblocks and the next call restarts
// a fresh process.
func (e *ExifTool) read(ctx context.Context) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := readUntilReady(e.stdout)
		ch <- result{data, err}
	}()

	select {
	case <-ctx.Done():
		e.stop()
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			if tail := e.stderr.string(); tail != "" {
				return nil, fmt.Errorf("%w (exiftool stderr: %s)", r.err, tail)
			}
			return nil, r.err
		}
		return r.data, nil
	}
}

// readUntilReady reads lines until the stay_open ready marker, returning
// everything before it (the JSON payload).
func readUntilReady(r *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if len(line) > 0 && strings.TrimRight(line, "\r\n") == readyMarker {
				return buf.Bytes(), nil
			}
			return nil, fmt.Errorf("reading exiftool output: %w", err)
		}
		if strings.TrimRight(line, "\r\n") == readyMarker {
			return buf.Bytes(), nil
		}
		buf.WriteString(line)
	}
}

// stop force-terminates the process and clears its handles. Must be called with
// e.mu held. Errors are ignored: the goal is simply to get back to a clean,
// restartable state.
func (e *ExifTool) stop() {
	if e.cmd == nil {
		return
	}
	if e.stdin != nil {
		_ = e.stdin.Close()
	}
	if e.cmd.Process != nil {
		_ = e.cmd.Process.Kill()
	}
	_ = e.cmd.Wait()
	e.cmd = nil
	e.stdin = nil
	e.stdout = nil
	e.stderr = nil
}

// Close shuts the process down gracefully by sending "-stay_open False" and
// waiting for it to exit. It is safe to call when no process is running and may
// be called more than once.
func (e *ExifTool) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.cmd == nil {
		return nil
	}
	// Best-effort graceful shutdown; fall back to killing on any write error.
	if _, err := io.WriteString(e.stdin, "-stay_open\nFalse\n"); err != nil {
		e.stop()
		return nil
	}
	_ = e.stdin.Close()
	err := e.cmd.Wait()
	e.cmd = nil
	e.stdin = nil
	e.stdout = nil
	e.stderr = nil
	if err != nil {
		return fmt.Errorf("closing exiftool: %w", err)
	}
	return nil
}

// ringBuffer keeps the most recent N lines of exiftool stderr for diagnostics.
type ringBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newRingBuffer(max int) *ringBuffer {
	return &ringBuffer{max: max}
}

func (rb *ringBuffer) add(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.lines = append(rb.lines, line)
	if len(rb.lines) > rb.max {
		rb.lines = rb.lines[len(rb.lines)-rb.max:]
	}
}

func (rb *ringBuffer) string() string {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return strings.Join(rb.lines, "; ")
}

// lookExiftool resolves the exiftool binary, preferring PATH and falling back to
// the known Homebrew location. It returns "" when no usable binary is found.
func lookExiftool() string {
	if p, err := exec.LookPath("exiftool"); err == nil {
		return p
	}
	if _, err := os.Stat(defaultExiftoolPath); err == nil {
		return defaultExiftoolPath
	}
	return ""
}
