// Package rclone implements a PAIM backup plugin that delegates all transfer,
// listing, and deletion to the external `rclone` binary. It turns any rclone
// remote — Google Drive, S3, Backblaze B2, SFTP, and dozens more — into a PAIM
// backup destination, so cloud backup requires no cloud SDKs in the app: the
// user configures the remote once with `rclone config`, and PAIM drives it.
//
// Every subprocess runs through an injected commandRunner so the package is
// testable without a real rclone (and without touching a real cloud account).
// The core backup package never imports this plugin; main.go registers it.
//
// Data-safety notes:
//   - Verify re-lists the uploaded object and compares size always, plus the
//     backend's content hash when it provides one (Google Drive = MD5). MD5 here
//     is chosen to MATCH the remote's checksum algorithm for an integrity
//     comparison — it is NOT a security choice — so crypto/md5 (stdlib) is fine.
//   - Delete uses `rclone deletefile`, which for Google Drive trashes rather than
//     hard-deletes by default (rclone's --drive-use-trash defaults to true). We
//     deliberately do NOT pass --drive-use-trash=false, aligning with PAIM's
//     never-hard-delete ethos wherever the backend supports a trash.
package rclone

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // MD5 matches Google Drive's checksum algorithm; integrity comparison, not security.
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sam-Lam/PAIM/internal/backup"
)

// PluginName is the stable identifier under which this plugin registers.
const PluginName = "rclone"

// defaultPath is used when a provider config omits a destination path: backups
// land under this folder at the remote root.
const defaultPath = "PAIM-Backup"

// authTimeout bounds the credential probe in Authenticate so an unreachable or
// stalled remote surfaces an error instead of hanging the worker.
const authTimeout = 30 * time.Second

// copyReadBuffer is the chunk size used while streaming a local file through the
// MD5 hasher during Verify (context is checked between chunks).
const copyReadBuffer = 1 << 20

// fallbackBinaries are probed, in order, when rclone is not on PATH. They are the
// standard Homebrew (Apple Silicon) and Intel/manual install locations.
var fallbackBinaries = []string{"/opt/homebrew/bin/rclone", "/usr/local/bin/rclone"}

// ErrNotInstalled is returned when no rclone binary can be located. The UI keys
// off it to show install guidance rather than a raw error.
var ErrNotInstalled = errors.New("rclone not installed — brew install rclone")

// Config is the JSON configuration for an rclone provider:
//
//	{"remote":"gdrive:","path":"PAIM-Backup","binary":""}
//
// remote is an rclone remote name (a trailing ":" is optional and normalized in).
// path is the destination folder within the remote (default "PAIM-Backup").
// binary optionally pins the rclone executable; empty means auto-discover.
type Config struct {
	Remote string `json:"remote"`
	Path   string `json:"path"`
	Binary string `json:"binary"`
}

// commandRunner executes an rclone subprocess. It is injected so tests never need
// a real rclone binary. Run streams each stderr line to onStderrLine as it
// arrives (used for JSON progress parsing), collects stdout and stderr in full,
// and returns them with any run error. A nil onStderrLine discards the callbacks.
// Implementations MUST kill the subprocess when ctx is cancelled and return
// ctx.Err() in that case.
type commandRunner interface {
	Run(ctx context.Context, binary string, args []string, onStderrLine func(line string)) (stdout, stderr []byte, err error)
}

// execRunner is the production commandRunner: it spawns rclone via
// exec.CommandContext (which kills the process when ctx is cancelled).
type execRunner struct{}

func (execRunner) Run(ctx context.Context, binary string, args []string, onStderrLine func(string)) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("rclone: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("rclone: start %s: %w", binary, err)
	}

	var stderr bytes.Buffer
	scanner := bufio.NewScanner(stderrPipe)
	// rclone JSON log lines (with the full stats blob) can be long; grow the
	// scanner buffer so a stats line is never split or dropped.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		stderr.WriteString(line)
		stderr.WriteByte('\n')
		if onStderrLine != nil {
			onStderrLine(line)
		}
	}
	waitErr := cmd.Wait()
	// A cancelled context takes precedence: report cancellation, not the
	// resulting "signal: killed" wait error.
	if ctx.Err() != nil {
		return stdout.Bytes(), stderr.Bytes(), ctx.Err()
	}
	return stdout.Bytes(), stderr.Bytes(), waitErr
}

// Plugin is an rclone-backed backup.Plugin.
type Plugin struct {
	runner commandRunner
	log    *slog.Logger

	// Configured state (set by Initialize).
	binary string // resolved rclone executable
	remote string // normalized remote, always ending in ":" (e.g. "gdrive:")
	path   string // destination folder within the remote (may be empty = root)

	// Test seams for binary discovery; default to the real implementations.
	lookPath   func(string) (string, error)
	fileExists func(string) bool
}

// New returns an unconfigured rclone plugin. It is the backup.Factory for this
// plugin; register it with reg.Register(rclone.PluginName, rclone.New).
func New() backup.Plugin {
	return &Plugin{
		runner:     execRunner{},
		log:        slog.Default(),
		lookPath:   exec.LookPath,
		fileExists: defaultFileExists,
	}
}

var _ backup.Plugin = (*Plugin)(nil)

func defaultFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// Name returns the plugin identifier.
func (p *Plugin) Name() string { return PluginName }

// Capabilities reports that rclone verifies (size + backend hash) and deletes
// (trashing where the backend supports it), does not resume interrupted uploads
// (an interrupted copyto is retried whole), and imposes no maximum file size.
func (p *Plugin) Capabilities() backup.Capabilities {
	return backup.Capabilities{
		SupportsVerify: true,
		SupportsDelete: true,
		SupportsResume: false,
		MaxFileSize:    0,
	}
}

// Initialize parses the config, resolves the rclone binary (a missing binary is
// ErrNotInstalled with install guidance), and validates the remote name against
// `rclone listremotes` so a typo is caught before any upload is attempted.
func (p *Plugin) Initialize(ctx context.Context, configJSON string) error {
	var cfg Config
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("rclone: parse config: %w", err)
	}
	if strings.TrimSpace(cfg.Remote) == "" {
		return errors.New("rclone: config remote is required")
	}

	binary, err := discoverBinary(cfg.Binary, p.lookPath, p.fileExists)
	if err != nil {
		return err
	}

	remote := normalizeRemote(cfg.Remote)
	remotes, err := listRemotes(ctx, p.runner, binary)
	if err != nil {
		return fmt.Errorf("rclone: list remotes: %w", err)
	}
	if !containsRemote(remotes, remote) {
		return fmt.Errorf("rclone: remote %q is not configured (run `rclone config`); known remotes: %s",
			remote, strings.Join(remotes, ", "))
	}

	path := strings.Trim(strings.TrimSpace(cfg.Path), "/")
	if path == "" {
		path = defaultPath
	}

	p.binary = binary
	p.remote = remote
	p.path = path
	return nil
}

// Authenticate probes the remote's credentials with `rclone about` (falling back
// to `rclone lsd` for backends without About support). An expired OAuth token
// (common with Google Drive) is mapped to a clear "reconnect" instruction.
func (p *Plugin) Authenticate(ctx context.Context) error {
	if p.binary == "" || p.remote == "" {
		return errors.New("rclone: plugin not initialized")
	}
	actx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()

	_, stderr, err := p.runner.Run(actx, p.binary, []string{"about", p.remote}, nil)
	if err == nil {
		return nil
	}
	// Some backends do not implement About; fall back to a cheap directory list.
	if isAboutUnsupported(stderr) {
		_, lsStderr, lsErr := p.runner.Run(actx, p.binary, []string{"lsd", p.remote}, nil)
		if lsErr == nil {
			return nil
		}
		err, stderr = lsErr, lsStderr
	}
	if isAuthError(stderr) {
		return fmt.Errorf("rclone: credentials for %s expired — run `rclone config reconnect %s`", p.remote, p.remote)
	}
	return fmt.Errorf("rclone: authenticate %s: %w: %s", p.remote, err, strings.TrimSpace(string(stderr)))
}

// Upload copies localPath to the remote via `rclone copyto`, streaming per-file
// byte progress parsed from rclone's JSON stats log. The context cancels (kills)
// the subprocess. Progress parsing is defensive: a start (0/size) and end
// (size/size) callback bracket the transfer so progress is reported even when no
// stats line could be parsed.
func (p *Plugin) Upload(ctx context.Context, localPath, remoteRelPath string, progressFn func(bytesDone, bytesTotal int64)) error {
	if p.binary == "" || p.remote == "" {
		return errors.New("rclone: plugin not initialized")
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("rclone: stat source %q: %w", localPath, err)
	}
	total := info.Size()
	dst := p.remotePath(remoteRelPath)

	if progressFn != nil {
		progressFn(0, total)
	}

	onLine := func(line string) {
		if progressFn == nil {
			return
		}
		if done, t, ok := parseStatsLine(line); ok {
			if t <= 0 {
				t = total
			}
			progressFn(done, t)
		}
	}

	args := []string{
		"copyto",
		"--no-traverse",
		"--use-json-log",
		"--stats", "250ms",
		"--stats-log-level", "NOTICE",
		localPath, dst,
	}
	_, stderr, err := p.runner.Run(ctx, p.binary, args, onLine)
	if err != nil {
		return fmt.Errorf("rclone: copyto %q -> %q: %w: %s", localPath, dst, err, lastLine(stderr))
	}

	if progressFn != nil {
		progressFn(total, total)
	}
	return nil
}

// Verify re-lists the uploaded object with `rclone lsjson --hash` and compares
// it to the local file. Size is always compared. When the backend supplies a
// content hash (Google Drive returns MD5), the local file's MD5 is computed
// streaming and compared; when the backend returns no usable hash the comparison
// is size-only and that is logged. A mismatch returns (false, nil) per the
// Plugin contract; a missing remote object also returns (false, nil).
func (p *Plugin) Verify(ctx context.Context, localPath, remoteRelPath string) (bool, error) {
	if p.binary == "" || p.remote == "" {
		return false, errors.New("rclone: plugin not initialized")
	}
	dst := p.remotePath(remoteRelPath)

	stdout, stderr, err := p.runner.Run(ctx, p.binary, []string{"lsjson", "--hash", dst}, nil)
	if err != nil {
		if isNotFound(stderr) {
			return false, nil
		}
		return false, fmt.Errorf("rclone: lsjson %q: %w: %s", dst, err, lastLine(stderr))
	}

	entry, ok := parseLsjson(stdout, path.Base(remoteRelPath))
	if !ok {
		// No entry for the object: treat as not present rather than an error.
		return false, nil
	}

	localSize, localMD5Fn, err := statLocal(localPath)
	if err != nil {
		return false, fmt.Errorf("rclone: stat local %q: %w", localPath, err)
	}
	if entry.Size != localSize {
		p.log.Warn("rclone: verify size mismatch", "remote", dst, "localSize", localSize, "remoteSize", entry.Size)
		return false, nil
	}

	remoteMD5 := entry.md5()
	if remoteMD5 == "" {
		p.log.Info("rclone: backend returned no hash; verified by size only", "remote", dst, "size", localSize)
		return true, nil
	}

	localMD5, err := localMD5Fn(ctx)
	if err != nil {
		return false, fmt.Errorf("rclone: md5 local %q: %w", localPath, err)
	}
	if !strings.EqualFold(localMD5, remoteMD5) {
		p.log.Warn("rclone: verify hash mismatch", "remote", dst)
		return false, nil
	}
	return true, nil
}

// Delete removes the object with `rclone deletefile`. On Google Drive this
// trashes the file (rclone's --drive-use-trash defaults to true); we do NOT pass
// --drive-use-trash=false, so on backends with a trash the delete is reversible,
// consistent with PAIM's never-hard-delete ethos. A missing object is treated as
// already deleted (no error).
func (p *Plugin) Delete(ctx context.Context, remoteRelPath string) error {
	if p.binary == "" || p.remote == "" {
		return errors.New("rclone: plugin not initialized")
	}
	dst := p.remotePath(remoteRelPath)
	_, stderr, err := p.runner.Run(ctx, p.binary, []string{"deletefile", dst}, nil)
	if err != nil {
		if isNotFound(stderr) {
			return nil
		}
		return fmt.Errorf("rclone: deletefile %q: %w: %s", dst, err, lastLine(stderr))
	}
	return nil
}

// remotePath joins the configured remote + path with the object's relative path
// into an rclone "remote:folder/rel" argument (always forward-slashed).
func (p *Plugin) remotePath(remoteRelPath string) string {
	parts := make([]string, 0, 2)
	if p.path != "" {
		parts = append(parts, p.path)
	}
	rel := strings.TrimLeft(filepath.ToSlash(remoteRelPath), "/")
	if rel != "" {
		parts = append(parts, rel)
	}
	return p.remote + strings.Join(parts, "/")
}

// ListRemotes returns the rclone remotes configured for the given binary (empty
// binary auto-discovers). It is exported for the UI so the Add-destination flow
// can present a remotes dropdown without constructing a Plugin.
func ListRemotes(ctx context.Context, binary string) ([]string, error) {
	resolved, err := ResolveBinary(binary)
	if err != nil {
		return nil, err
	}
	return listRemotes(ctx, execRunner{}, resolved)
}

// ResolveBinary locates the rclone executable using the standard discovery order
// (explicit override → PATH → Homebrew/Intel fallbacks), returning ErrNotInstalled
// when none is found. Exported for the service layer's install-status probe.
func ResolveBinary(binary string) (string, error) {
	return discoverBinary(binary, exec.LookPath, defaultFileExists)
}

// discoverBinary resolves the rclone executable: an explicit config binary wins,
// then PATH (via lookPath), then the known fallback install locations (via
// exists). ErrNotInstalled is returned when nothing is found.
func discoverBinary(configBinary string, lookPath func(string) (string, error), exists func(string) bool) (string, error) {
	if b := strings.TrimSpace(configBinary); b != "" {
		return b, nil
	}
	if p, err := lookPath("rclone"); err == nil {
		return p, nil
	}
	for _, cand := range fallbackBinaries {
		if exists(cand) {
			return cand, nil
		}
	}
	return "", ErrNotInstalled
}

// listRemotes runs `rclone listremotes` and returns the normalized remote names.
func listRemotes(ctx context.Context, runner commandRunner, binary string) ([]string, error) {
	stdout, stderr, err := runner.Run(ctx, binary, []string{"listremotes"}, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, lastLine(stderr))
	}
	var out []string
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, normalizeRemote(line))
	}
	return out, nil
}

// normalizeRemote strips any whitespace and trailing colons, then appends exactly
// one colon, so "gdrive", "gdrive:" and " gdrive: " all become "gdrive:".
func normalizeRemote(remote string) string {
	name := strings.TrimRight(strings.TrimSpace(remote), ":")
	return name + ":"
}

func containsRemote(remotes []string, want string) bool {
	for _, r := range remotes {
		if r == want {
			return true
		}
	}
	return false
}

// statsLine is the subset of an rclone --use-json-log stats line we care about.
type statsLine struct {
	Stats *struct {
		Bytes      int64 `json:"bytes"`
		TotalBytes int64 `json:"totalBytes"`
	} `json:"stats"`
}

// parseStatsLine defensively extracts (bytesDone, bytesTotal) from one rclone
// JSON log line. ok is false for non-JSON lines and JSON lines without a stats
// object, so callers simply skip them.
func parseStatsLine(line string) (done, total int64, ok bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return 0, 0, false
	}
	var s statsLine
	if err := json.Unmarshal([]byte(line), &s); err != nil || s.Stats == nil {
		return 0, 0, false
	}
	return s.Stats.Bytes, s.Stats.TotalBytes, true
}

// lsjsonEntry is one item from `rclone lsjson --hash` output.
type lsjsonEntry struct {
	Path   string            `json:"Path"`
	Name   string            `json:"Name"`
	Size   int64             `json:"Size"`
	IsDir  bool              `json:"IsDir"`
	Hashes map[string]string `json:"Hashes"`
}

func (e lsjsonEntry) md5() string {
	if e.Hashes == nil {
		return ""
	}
	return e.Hashes["md5"]
}

// parseLsjson parses lsjson output (a JSON array, or a single --stat object as a
// defensive fallback) and returns the entry for wantName, or the sole entry when
// only one is present. ok is false when the object is absent.
func parseLsjson(stdout []byte, wantName string) (lsjsonEntry, bool) {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return lsjsonEntry{}, false
	}
	var entries []lsjsonEntry
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &entries); err != nil {
			return lsjsonEntry{}, false
		}
	} else {
		var single lsjsonEntry
		if err := json.Unmarshal(trimmed, &single); err != nil {
			return lsjsonEntry{}, false
		}
		entries = []lsjsonEntry{single}
	}

	var files []lsjsonEntry
	for _, e := range entries {
		if !e.IsDir {
			files = append(files, e)
		}
	}
	switch {
	case len(files) == 0:
		return lsjsonEntry{}, false
	case len(files) == 1:
		return files[0], true
	default:
		for _, e := range files {
			if e.Name == wantName || path.Base(e.Path) == wantName {
				return e, true
			}
		}
		return lsjsonEntry{}, false
	}
}

// statLocal returns the local file size and a lazy, context-aware MD5 computation
// (only invoked when a hash comparison is actually needed).
func statLocal(path string) (int64, func(ctx context.Context) (string, error), error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, nil, err
	}
	md5Fn := func(ctx context.Context) (string, error) { return fileMD5(ctx, path) }
	return info.Size(), md5Fn, nil
}

// fileMD5 streams a file through crypto/md5, checking ctx between chunks so a
// cancelled Verify aborts promptly. MD5 is used to match the remote backend's
// checksum algorithm (e.g. Google Drive), not for security.
func fileMD5(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := md5.New() //nolint:gosec // integrity comparison against the remote's MD5, not security.
	buf := make([]byte, copyReadBuffer)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// isAboutUnsupported reports whether an `rclone about` failure is because the
// backend does not implement About (so a fallback list is worth trying).
func isAboutUnsupported(stderr []byte) bool {
	s := strings.ToLower(string(stderr))
	return strings.Contains(s, "doesn't support about") ||
		strings.Contains(s, "does not support about") ||
		strings.Contains(s, "not supported")
}

// isAuthError reports whether an rclone failure looks like an expired/invalid
// credential (so we can suggest `rclone config reconnect`).
func isAuthError(stderr []byte) bool {
	s := strings.ToLower(string(stderr))
	return strings.Contains(s, "token") ||
		strings.Contains(s, "oauth") ||
		strings.Contains(s, "unauthenticated") ||
		strings.Contains(s, "unauthorized") ||
		strings.Contains(s, "401") ||
		strings.Contains(s, "invalid_grant")
}

// isNotFound reports whether an rclone failure indicates a missing object.
func isNotFound(stderr []byte) bool {
	s := strings.ToLower(string(stderr))
	return strings.Contains(s, "not found") ||
		strings.Contains(s, "directory not found") ||
		strings.Contains(s, "object not found") ||
		strings.Contains(s, "no such file")
}

// lastLine returns the last non-empty line of captured stderr for concise error
// messages (rclone prints multi-line diagnostics; the tail is the useful part).
func lastLine(stderr []byte) string {
	lines := strings.Split(strings.TrimRight(string(stderr), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}
