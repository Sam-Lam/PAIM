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
	"sync"
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
//	{"remotes":["gp1:","gp2:"],"path":"PAIM-Backup","mirror":true}
//
// remote is a single rclone remote name (a trailing ":" is optional). remotes is
// an ordered POOL of remotes for a mirror provider: each remote is typically
// backed by a different Google Cloud project (and thus an independent daily
// quota), and the plugin fails over to the next remote when one is exhausted,
// roughly multiplying daily upload throughput. A single remote is equivalent to a
// one-element pool. path is the destination folder within each remote (default
// "PAIM-Backup"). mirror marks the provider as quality-of-life; a pool (>1 remote)
// is only permitted for a mirror provider because Google Photos' app-scoped API
// means one remote cannot see another's uploads, so verify/delete are best-effort.
// binary optionally pins the rclone executable; empty means auto-discover.
type Config struct {
	Remote  string   `json:"remote"`
	Remotes []string `json:"remotes"`
	Path    string   `json:"path"`
	Mirror  bool     `json:"mirror"`
	Binary  string   `json:"binary"`
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
	binary  string   // resolved rclone executable
	remotes []string // normalized remote pool, in order; each ends in ":" (>=1)
	path    string   // destination folder within each remote (may be empty = root)
	mirror  bool     // quality-of-life provider (required for a >1 remote pool)

	// throttled marks pool remotes whose backend needs conservative transfer flags
	// (e.g. Google Photos). Keyed by normalized remote.
	throttled map[string]bool

	// gphotos marks pool remotes whose backend is Google Photos, whose upload paths
	// must be mapped into the album/ virtual root (see remotePathFor). Keyed by
	// normalized remote.
	gphotos map[string]bool

	// cooldownMu guards remoteCooldowns: the per-remote quota cooldown map that
	// drives pool failover. It lives on the plugin instance, and the Manager caches
	// one plugin instance per provider, so the cooldown state persists across a
	// provider's uploads for the lifetime of the process.
	cooldownMu      sync.Mutex
	remoteCooldowns map[string]time.Time
	// now is the clock used for cooldown bookkeeping (injectable for tests).
	now func() time.Time

	// Test seams for binary discovery; default to the real implementations.
	lookPath   func(string) (string, error)
	fileExists func(string) bool
}

// New returns an unconfigured rclone plugin. It is the backup.Factory for this
// plugin; register it with reg.Register(rclone.PluginName, rclone.New).
func New() backup.Plugin {
	return &Plugin{
		runner:          execRunner{},
		log:             slog.Default(),
		throttled:       make(map[string]bool),
		gphotos:         make(map[string]bool),
		remoteCooldowns: make(map[string]time.Time),
		now:             time.Now,
		lookPath:        exec.LookPath,
		fileExists:      defaultFileExists,
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
// ErrNotInstalled with install guidance), validates every remote in the pool
// against `rclone listremotes` so a typo is caught before any upload, rejects a
// multi-remote pool on a non-mirror provider, and probes each remote's backend
// type (so uploads to throttled backends like Google Photos get conservative
// flags).
func (p *Plugin) Initialize(ctx context.Context, configJSON string) error {
	var cfg Config
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("rclone: parse config: %w", err)
	}

	pool := poolFromConfig(cfg)
	if len(pool) == 0 {
		return errors.New("rclone: config remote is required")
	}
	if len(pool) > 1 && !cfg.Mirror {
		return errors.New("rclone: a multi-remote pool is only allowed on a mirror (quality-of-life) provider — Google Photos' app-scoped API means one remote cannot see another's uploads")
	}

	binary, err := discoverBinary(cfg.Binary, p.lookPath, p.fileExists)
	if err != nil {
		return err
	}

	known, err := listRemotes(ctx, p.runner, binary)
	if err != nil {
		return fmt.Errorf("rclone: list remotes: %w", err)
	}
	throttled := make(map[string]bool, len(pool))
	gphotos := make(map[string]bool, len(pool))
	for _, remote := range pool {
		if !containsRemote(known, remote) {
			return fmt.Errorf("rclone: remote %q is not configured (run `rclone config`); known remotes: %s",
				remote, strings.Join(known, ", "))
		}
		// Best-effort backend-type probe: failure to detect just means no
		// conservative flags and no album/ mapping (the upload path is used verbatim),
		// so it is not fatal.
		if bt, berr := detectBackendType(ctx, p.runner, binary, remote); berr == nil {
			if backendNeedsThrottle(bt) {
				throttled[remote] = true
			}
			if backendIsGooglePhotos(bt) {
				gphotos[remote] = true
			}
		}
	}

	path := strings.Trim(strings.TrimSpace(cfg.Path), "/")
	if path == "" {
		path = defaultPath
	}

	p.binary = binary
	p.remotes = pool
	p.path = path
	p.mirror = cfg.Mirror
	p.throttled = throttled
	p.gphotos = gphotos
	return nil
}

// poolFromConfig builds the ordered, normalized remote pool from a config,
// accepting either the pool field (remotes) or the legacy single remote, and
// de-duplicating while preserving order.
func poolFromConfig(cfg Config) []string {
	raw := cfg.Remotes
	if len(raw) == 0 && strings.TrimSpace(cfg.Remote) != "" {
		raw = []string{cfg.Remote}
	}
	seen := make(map[string]bool, len(raw))
	pool := make([]string, 0, len(raw))
	for _, r := range raw {
		if strings.TrimSpace(r) == "" {
			continue
		}
		n := normalizeRemote(r)
		if seen[n] {
			continue
		}
		seen[n] = true
		pool = append(pool, n)
	}
	return pool
}

// Authenticate probes the pool's credentials, succeeding as soon as any remote
// authenticates (a pool only needs one working remote to make progress). Each
// remote is probed with `rclone about` (falling back to `rclone lsd` for backends
// without About support). An expired OAuth token (common with Google Drive) is
// mapped to a clear "reconnect" instruction on the last failing remote.
func (p *Plugin) Authenticate(ctx context.Context) error {
	if p.binary == "" || len(p.remotes) == 0 {
		return errors.New("rclone: plugin not initialized")
	}
	actx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()

	var lastErr error
	var lastStderr []byte
	var lastRemote string
	for _, remote := range p.remotes {
		if err := p.authenticateRemote(actx, remote); err == nil {
			return nil
		} else {
			lastErr, lastStderr, lastRemote = err.err, err.stderr, remote
		}
	}
	if isAuthError(lastStderr) {
		return fmt.Errorf("rclone: credentials for %s expired — run `rclone config reconnect %s`", lastRemote, lastRemote)
	}
	return fmt.Errorf("rclone: authenticate %s: %w: %s", lastRemote, lastErr, strings.TrimSpace(string(lastStderr)))
}

// authError carries a single remote's authentication failure detail.
type authError struct {
	err    error
	stderr []byte
}

// authenticateRemote probes one remote; a nil return means it authenticated.
func (p *Plugin) authenticateRemote(ctx context.Context, remote string) *authError {
	_, stderr, err := p.runner.Run(ctx, p.binary, []string{"about", remote}, nil)
	if err == nil {
		return nil
	}
	if isAboutUnsupported(stderr) {
		_, lsStderr, lsErr := p.runner.Run(ctx, p.binary, []string{"lsd", remote}, nil)
		if lsErr == nil {
			return nil
		}
		err, stderr = lsErr, lsStderr
	}
	return &authError{err: err, stderr: stderr}
}

// Upload copies localPath to the remote pool via `rclone copyto`, streaming
// per-file byte progress parsed from rclone's JSON stats log. It tries the pool
// remotes in order, skipping any currently in per-remote quota cooldown; if a
// remote returns a quota/rate error it is put into cooldown and the SAME file is
// retried on the next remote (one failover chain per call, bounded by the pool
// size). Only when EVERY remote is cooling does it return backup.ErrProvider
// Cooldown (with the soonest remote's expiry) so the Manager engages the
// provider-wide cooldown. A non-quota error fails the upload immediately. The
// context cancels (kills) the subprocess.
func (p *Plugin) Upload(ctx context.Context, localPath, remoteRelPath string, progressFn func(bytesDone, bytesTotal int64)) error {
	if p.binary == "" || len(p.remotes) == 0 {
		return errors.New("rclone: plugin not initialized")
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("rclone: stat source %q: %w", localPath, err)
	}
	total := info.Size()

	if progressFn != nil {
		progressFn(0, total)
	}

	var soonest time.Time
	for _, remote := range p.remotes {
		if until, cooling := p.remoteCooling(remote); cooling {
			soonest = earliest(soonest, until)
			continue
		}
		stderr, err := p.uploadTo(ctx, remote, localPath, remoteRelPath, total, progressFn)
		if err == nil {
			if progressFn != nil {
				progressFn(total, total)
			}
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		if cd, ok := classifyCooldown(string(stderr), p.now()); ok {
			until := p.now().Add(cd.RetryAfter)
			p.markRemoteCooling(remote, until)
			soonest = earliest(soonest, until)
			p.log.Warn("rclone: remote hit quota; failing over",
				"remote", remote, "reason", cd.Reason, "resumesAt", until.Format(time.RFC3339))
			continue // try the next remote for the same file
		}
		return err // non-quota error: fail this upload
	}

	// Every remote is cooling: surface a provider-wide cooldown to the Manager so
	// it stops claiming this provider's jobs until the soonest remote clears.
	if !soonest.IsZero() {
		return &backup.ErrProviderCooldown{
			RetryAfter: soonestDuration(p.now(), soonest),
			Reason:     "all pool remotes are in quota cooldown",
		}
	}
	return errors.New("rclone: no remotes available for upload")
}

// uploadTo runs `rclone copyto` to a single remote, returning the captured stderr
// (for cooldown classification) and any run error.
func (p *Plugin) uploadTo(ctx context.Context, remote, localPath, remoteRelPath string, total int64, progressFn func(bytesDone, bytesTotal int64)) ([]byte, error) {
	dst := p.remotePathFor(remote, remoteRelPath)

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
	}
	if p.throttled[remote] {
		// Conservative flags for tight per-second write limits (e.g. Google Photos):
		// one transfer at a time and at most 2 transactions/second.
		args = append(args, "--tpslimit", "2", "--transfers", "1")
	}
	args = append(args, localPath, dst)

	_, stderr, err := p.runner.Run(ctx, p.binary, args, onLine)
	if err != nil {
		return stderr, fmt.Errorf("rclone: copyto %q -> %q: %w: %s", localPath, dst, err, lastLine(stderr))
	}
	return stderr, nil
}

// earliest returns the earlier of a (possibly zero) accumulator and t.
func earliest(acc, t time.Time) time.Time {
	if acc.IsZero() || t.Before(acc) {
		return t
	}
	return acc
}

// soonestDuration returns the non-negative duration from now until t.
func soonestDuration(now, t time.Time) time.Duration {
	d := t.Sub(now)
	if d < 0 {
		d = 0
	}
	return d
}

// remoteCooling reports whether a remote is currently in per-remote cooldown, and
// until when. Expired entries are pruned.
func (p *Plugin) remoteCooling(remote string) (time.Time, bool) {
	p.cooldownMu.Lock()
	defer p.cooldownMu.Unlock()
	until, ok := p.remoteCooldowns[remote]
	if !ok {
		return time.Time{}, false
	}
	if !until.After(p.now()) {
		delete(p.remoteCooldowns, remote)
		return time.Time{}, false
	}
	return until, true
}

// markRemoteCooling records that a remote is cooling until the given time.
func (p *Plugin) markRemoteCooling(remote string, until time.Time) {
	p.cooldownMu.Lock()
	if prev, ok := p.remoteCooldowns[remote]; !ok || until.After(prev) {
		p.remoteCooldowns[remote] = until
	}
	p.cooldownMu.Unlock()
}

// Verify re-lists the uploaded object with `rclone lsjson --hash` and compares
// it to the local file. Size is always compared. When the backend supplies a
// content hash (Google Drive returns MD5), the local file's MD5 is computed
// streaming and compared; when the backend returns no usable hash the comparison
// is size-only and that is logged. A mismatch returns (false, nil) per the
// Plugin contract; a missing remote object also returns (false, nil).
func (p *Plugin) Verify(ctx context.Context, localPath, remoteRelPath string) (bool, error) {
	if p.binary == "" || len(p.remotes) == 0 {
		return false, errors.New("rclone: plugin not initialized")
	}

	// For a pool, attempt verification on each remote until one confirms the object
	// (its own upload) or all miss. Google Photos' app-scoped API means remote B
	// cannot see remote A's uploads, so a miss on every remote is treated as
	// "unavailable" (false, nil) rather than a failure — the Manager's best-effort
	// mirror path completes such jobs with a note.
	var lastErr error
	for _, remote := range p.remotes {
		ok, err := p.verifyOn(ctx, remote, localPath, remoteRelPath)
		if err != nil {
			lastErr = err
			continue
		}
		if ok {
			return true, nil
		}
	}
	if lastErr != nil && !p.mirror {
		return false, lastErr
	}
	return false, nil
}

// verifyOn verifies the object on a single remote (size always, plus the backend
// MD5 when available). A missing object returns (false, nil).
func (p *Plugin) verifyOn(ctx context.Context, remote, localPath, remoteRelPath string) (bool, error) {
	dst := p.remotePathFor(remote, remoteRelPath)

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
	if p.binary == "" || len(p.remotes) == 0 {
		return errors.New("rclone: plugin not initialized")
	}
	// Best-effort across the pool: the object may live on any one remote (whichever
	// uploaded it), and a pool is only used for mirrors, so deletion is convenience.
	// Succeed if any remote deletes it (or reports it absent); return the last error
	// only if every remote errored.
	var lastErr error
	for _, remote := range p.remotes {
		dst := p.remotePathFor(remote, remoteRelPath)
		_, stderr, err := p.runner.Run(ctx, p.binary, []string{"deletefile", dst}, nil)
		if err == nil || isNotFound(stderr) {
			lastErr = nil
			if err == nil {
				return nil
			}
			continue
		}
		lastErr = fmt.Errorf("rclone: deletefile %q: %w: %s", dst, err, lastLine(stderr))
	}
	return lastErr
}

// remotePathFor joins one remote + the configured path with the object's relative
// path into an rclone "remote:folder/rel" argument (always forward-slashed). Upload,
// Verify, and Delete all resolve their destination through here, so the mapping is
// consistent across the three.
//
// GOOGLE PHOTOS MAPPING: rclone's gphotos backend is a virtual filesystem — it only
// accepts writes under two roots, `album/<album name>/...` and `upload/...`. A plain
// "PAIM-Backup/2019/…/IMG.jpg" targets a nonexistent root and errors. For a Google
// Photos remote we therefore map the object under `album/`, using the object's
// DIRECTORY path as the (nested) album name and the basename as the media item:
//
//	path "PAIM-Backup" + rel "2019/2019-06-12 Yosemite/DSCF0001.JPG"
//	  -> remote:album/PAIM-Backup/2019/2019-06-12 Yosemite/DSCF0001.JPG
//	  (album name "PAIM-Backup/2019/2019-06-12 Yosemite", item "DSCF0001.JPG")
//
// Slashes are legal inside a Photos album name, and gphotos treats the whole nested
// path under album/ as one album — so each date folder becomes its own album. That
// yields per-EVENT albums, which also sidesteps Google Photos' ~20,000-item-per-album
// cap (a single flat album would overflow on a large library). A configured path that
// already starts with `album/` or `upload` is respected verbatim (power users who
// know the gphotos root layout).
func (p *Plugin) remotePathFor(remote, remoteRelPath string) string {
	effectivePath := p.path
	if p.gphotos[remote] {
		effectivePath = gphotosPath(p.path)
	}
	parts := make([]string, 0, 2)
	if effectivePath != "" {
		parts = append(parts, effectivePath)
	}
	rel := strings.TrimLeft(filepath.ToSlash(remoteRelPath), "/")
	if rel != "" {
		parts = append(parts, rel)
	}
	return remote + strings.Join(parts, "/")
}

// gphotosPath maps a configured destination path into a Google Photos virtual root.
// A path already rooted at album/ or upload/ is respected verbatim; anything else is
// prefixed with "album/" so its date subfolders become nested album names (see
// remotePathFor). The path has already been trimmed of surrounding slashes by
// Initialize.
func gphotosPath(configured string) string {
	c := strings.Trim(configured, "/")
	lower := strings.ToLower(c)
	if lower == "upload" || strings.HasPrefix(lower, "upload/") ||
		lower == "album" || strings.HasPrefix(lower, "album/") {
		return c
	}
	if c == "" {
		return "album"
	}
	return "album/" + c
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
