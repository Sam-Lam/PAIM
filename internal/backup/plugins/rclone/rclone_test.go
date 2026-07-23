// Internal test package (package rclone) so tests can inject the unexported
// commandRunner and binary-discovery seams without a real rclone binary. One
// optional integration test (TestIntegrationRealRclone) runs only when a real
// rclone AND PAIM_RCLONE_TEST_REMOTE are present.
package rclone

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner is an injectable commandRunner. Each call is matched to a scripted
// response by the first arg (the rclone subcommand); the invocation is recorded.
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	// responder returns stdout, stderr, err for a given invocation and may emit
	// stderr lines through onStderrLine (to exercise progress parsing).
	responder func(ctx context.Context, args []string, onStderrLine func(string)) (stdout, stderr []byte, err error)
}

func (f *fakeRunner) Run(ctx context.Context, binary string, args []string, onStderrLine func(string)) ([]byte, []byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{binary}, args...))
	f.mu.Unlock()
	if f.responder == nil {
		return nil, nil, nil
	}
	return f.responder(ctx, args, onStderrLine)
}

func (f *fakeRunner) lastCall() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeRunner) callWith(sub string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if len(c) >= 2 && c[1] == sub {
			return c
		}
	}
	return nil
}

// newTestPlugin builds a plugin wired to a fake runner and stubbed discovery so
// it never touches PATH or the filesystem for the binary.
func newTestPlugin(runner *fakeRunner) *Plugin {
	p := New().(*Plugin)
	p.runner = runner
	p.lookPath = func(string) (string, error) { return "/fake/rclone", nil }
	p.fileExists = func(string) bool { return true }
	return p
}

// listRemotesResponder answers `listremotes` with the given remotes and defers
// everything else to next.
func listRemotesResponder(remotes []string, next func(ctx context.Context, args []string, onStderrLine func(string)) ([]byte, []byte, error)) func(context.Context, []string, func(string)) ([]byte, []byte, error) {
	return func(ctx context.Context, args []string, onLine func(string)) ([]byte, []byte, error) {
		if len(args) > 0 && args[0] == "listremotes" {
			return []byte(strings.Join(remotes, "\n") + "\n"), nil, nil
		}
		if next != nil {
			return next(ctx, args, onLine)
		}
		return nil, nil, nil
	}
}

func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

// ---- Config parsing & remote validation ----

func TestInitializeParsesConfigAndValidatesRemote(t *testing.T) {
	runner := &fakeRunner{responder: listRemotesResponder([]string{"gdrive:", "b2:"}, nil)}
	p := newTestPlugin(runner)

	cfg := `{"remote":"gdrive:","path":"PAIM-Backup"}`
	if err := p.Initialize(context.Background(), cfg); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if p.remote != "gdrive:" {
		t.Errorf("remote = %q, want gdrive:", p.remote)
	}
	if p.path != "PAIM-Backup" {
		t.Errorf("path = %q, want PAIM-Backup", p.path)
	}
	if p.binary != "/fake/rclone" {
		t.Errorf("binary = %q, want /fake/rclone", p.binary)
	}
}

func TestInitializeNormalizesRemoteWithoutColon(t *testing.T) {
	runner := &fakeRunner{responder: listRemotesResponder([]string{"gdrive:"}, nil)}
	p := newTestPlugin(runner)
	// Config remote lacks the trailing colon; listremotes has it — both normalize.
	if err := p.Initialize(context.Background(), `{"remote":"gdrive"}`); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if p.remote != "gdrive:" {
		t.Errorf("remote = %q, want gdrive:", p.remote)
	}
	if p.path != defaultPath {
		t.Errorf("path defaulted to %q, want %q", p.path, defaultPath)
	}
}

func TestInitializeRejectsUnknownRemote(t *testing.T) {
	runner := &fakeRunner{responder: listRemotesResponder([]string{"gdrive:"}, nil)}
	p := newTestPlugin(runner)
	err := p.Initialize(context.Background(), `{"remote":"typo:"}`)
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("expected unknown-remote error, got %v", err)
	}
}

func TestInitializeRejectsEmptyAndBadConfig(t *testing.T) {
	runner := &fakeRunner{responder: listRemotesResponder([]string{"gdrive:"}, nil)}
	if err := newTestPlugin(runner).Initialize(context.Background(), `{}`); err == nil {
		t.Fatalf("expected error for missing remote")
	}
	if err := newTestPlugin(runner).Initialize(context.Background(), `{bad json`); err == nil {
		t.Fatalf("expected error for malformed JSON")
	}
}

func TestInitializeMissingBinary(t *testing.T) {
	runner := &fakeRunner{responder: listRemotesResponder([]string{"gdrive:"}, nil)}
	p := New().(*Plugin)
	p.runner = runner
	p.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	p.fileExists = func(string) bool { return false }
	err := p.Initialize(context.Background(), `{"remote":"gdrive:"}`)
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("expected ErrNotInstalled, got %v", err)
	}
}

// ---- Binary discovery order ----

func TestDiscoverBinaryOrder(t *testing.T) {
	t.Run("explicit config wins", func(t *testing.T) {
		got, err := discoverBinary("/custom/rclone",
			func(string) (string, error) { t.Fatal("lookPath should not be called"); return "", nil },
			func(string) bool { t.Fatal("exists should not be called"); return false })
		if err != nil || got != "/custom/rclone" {
			t.Fatalf("got (%q,%v), want /custom/rclone", got, err)
		}
	})
	t.Run("PATH before fallbacks", func(t *testing.T) {
		got, err := discoverBinary("",
			func(string) (string, error) { return "/usr/bin/rclone", nil },
			func(string) bool { t.Fatal("exists should not be reached"); return false })
		if err != nil || got != "/usr/bin/rclone" {
			t.Fatalf("got (%q,%v), want /usr/bin/rclone", got, err)
		}
	})
	t.Run("homebrew fallback before intel", func(t *testing.T) {
		got, err := discoverBinary("",
			func(string) (string, error) { return "", errors.New("no PATH") },
			func(p string) bool { return p == "/opt/homebrew/bin/rclone" })
		if err != nil || got != "/opt/homebrew/bin/rclone" {
			t.Fatalf("got (%q,%v), want homebrew path", got, err)
		}
	})
	t.Run("intel fallback", func(t *testing.T) {
		got, err := discoverBinary("",
			func(string) (string, error) { return "", errors.New("no PATH") },
			func(p string) bool { return p == "/usr/local/bin/rclone" })
		if err != nil || got != "/usr/local/bin/rclone" {
			t.Fatalf("got (%q,%v), want intel path", got, err)
		}
	})
	t.Run("none found", func(t *testing.T) {
		_, err := discoverBinary("",
			func(string) (string, error) { return "", errors.New("no PATH") },
			func(string) bool { return false })
		if !errors.Is(err, ErrNotInstalled) {
			t.Fatalf("want ErrNotInstalled, got %v", err)
		}
	})
}

// ---- Upload arg construction + JSON progress parsing ----

func TestUploadArgsAndProgressParsing(t *testing.T) {
	src := filepath.Join(t.TempDir(), "IMG_0001.JPG")
	content := []byte("some photo bytes here")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	total := int64(len(content))

	// Realistic rclone --use-json-log stats lines (captured format) plus noise
	// lines that must be skipped without error.
	statsLines := []string{
		`{"level":"notice","msg":"starting","source":"x.go:1"}`, // no stats -> skipped
		fmt.Sprintf(`{"time":"2026-07-23T09:31:58Z","level":"notice","msg":"\nTransferred: ...","stats":{"bytes":%d,"totalBytes":%d,"transfers":0}}`, total/2, total),
		`not json at all`, // skipped
		fmt.Sprintf(`{"time":"2026-07-23T09:31:59Z","level":"notice","msg":"done","stats":{"bytes":%d,"totalBytes":%d,"transfers":1}}`, total, total),
	}

	runner := &fakeRunner{responder: listRemotesResponder([]string{"gdrive:"}, func(ctx context.Context, args []string, onLine func(string)) ([]byte, []byte, error) {
		if args[0] == "copyto" {
			for _, l := range statsLines {
				onLine(l)
			}
			return nil, nil, nil
		}
		return nil, nil, nil
	})}

	p := newTestPlugin(runner)
	if err := p.Initialize(context.Background(), `{"remote":"gdrive:","path":"PAIM-Backup"}`); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	var mu sync.Mutex
	var samples [][2]int64
	progress := func(done, tot int64) {
		mu.Lock()
		samples = append(samples, [2]int64{done, tot})
		mu.Unlock()
	}

	remote := "2024/2024-01-01/IMG_0001.JPG"
	if err := p.Upload(context.Background(), src, remote, progress); err != nil {
		t.Fatalf("upload: %v", err)
	}

	// Arg construction.
	call := runner.callWith("copyto")
	want := []string{"/fake/rclone", "copyto", "--no-traverse", "--use-json-log", "--stats", "250ms", "--stats-log-level", "NOTICE", src, "gdrive:PAIM-Backup/2024/2024-01-01/IMG_0001.JPG"}
	if strings.Join(call, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("copyto args =\n  %v\nwant\n  %v", call, want)
	}

	// Progress: starts at 0/total, includes the mid-transfer sample, ends at total/total.
	mu.Lock()
	defer mu.Unlock()
	if len(samples) < 3 {
		t.Fatalf("expected >=3 progress samples, got %v", samples)
	}
	if samples[0] != [2]int64{0, total} {
		t.Errorf("first sample = %v, want [0 %d]", samples[0], total)
	}
	if last := samples[len(samples)-1]; last != [2]int64{total, total} {
		t.Errorf("last sample = %v, want [%d %d]", last, total, total)
	}
	sawMid := false
	for _, s := range samples {
		if s == [2]int64{total / 2, total} {
			sawMid = true
		}
	}
	if !sawMid {
		t.Errorf("expected a mid-transfer sample %v in %v", [2]int64{total / 2, total}, samples)
	}
}

func TestUploadFallsBackToStartEndWhenNoStats(t *testing.T) {
	src := filepath.Join(t.TempDir(), "a.mov")
	content := []byte("0123456789")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	total := int64(len(content))

	runner := &fakeRunner{responder: listRemotesResponder([]string{"gdrive:"}, func(ctx context.Context, args []string, onLine func(string)) ([]byte, []byte, error) {
		// copyto emits nothing parseable.
		if args[0] == "copyto" {
			onLine("Transferred: some human text, not json")
		}
		return nil, nil, nil
	})}
	p := newTestPlugin(runner)
	if err := p.Initialize(context.Background(), `{"remote":"gdrive:","path":""}`); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	var samples [][2]int64
	if err := p.Upload(context.Background(), src, "a.mov", func(d, tt int64) {
		samples = append(samples, [2]int64{d, tt})
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if len(samples) != 2 || samples[0] != [2]int64{0, total} || samples[1] != [2]int64{total, total} {
		t.Fatalf("fallback samples = %v, want [[0 %d] [%d %d]]", samples, total, total, total)
	}
	// Empty config path defaults to PAIM-Backup (never silently the remote root).
	if got := runner.lastCall(); got[len(got)-1] != "gdrive:PAIM-Backup/a.mov" {
		t.Errorf("dst = %q, want gdrive:PAIM-Backup/a.mov", got[len(got)-1])
	}
}

func TestUploadErrorSurfacesStderrTail(t *testing.T) {
	src := filepath.Join(t.TempDir(), "a.jpg")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runner := &fakeRunner{responder: listRemotesResponder([]string{"gdrive:"}, func(ctx context.Context, args []string, onLine func(string)) ([]byte, []byte, error) {
		if args[0] == "copyto" {
			return nil, []byte("some log\nFatal error: quota exceeded\n"), errors.New("exit status 1")
		}
		return nil, nil, nil
	})}
	p := newTestPlugin(runner)
	if err := p.Initialize(context.Background(), `{"remote":"gdrive:"}`); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	err := p.Upload(context.Background(), src, "a.jpg", nil)
	if err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("expected error with stderr tail, got %v", err)
	}
}

// ---- Verify: size + hash match / mismatch / no-hash ----

func verifyRunner(remotes []string, lsjson []byte, lsErr error, lsStderr []byte) *fakeRunner {
	return &fakeRunner{responder: listRemotesResponder(remotes, func(ctx context.Context, args []string, onLine func(string)) ([]byte, []byte, error) {
		if args[0] == "lsjson" {
			return lsjson, lsStderr, lsErr
		}
		return nil, nil, nil
	})}
}

func initedVerifyPlugin(t *testing.T, runner *fakeRunner, cfgPath string) *Plugin {
	t.Helper()
	p := newTestPlugin(runner)
	cfg := fmt.Sprintf(`{"remote":"gdrive:","path":%q}`, cfgPath)
	if err := p.Initialize(context.Background(), cfg); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return p
}

func TestVerifySizeAndHashMatch(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f.bin")
	content := []byte("verify me exactly")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	lsjson := []byte(fmt.Sprintf(`[{"Path":"f.bin","Name":"f.bin","Size":%d,"IsDir":false,"Hashes":{"md5":"%s"}}]`,
		len(content), md5Hex(content)))
	p := initedVerifyPlugin(t, verifyRunner([]string{"gdrive:"}, lsjson, nil, nil), "PAIM-Backup")

	ok, err := p.Verify(context.Background(), src, "f.bin")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatalf("verify should pass on size+md5 match")
	}
	// lsjson pointed at the fully-joined remote path.
	if got := runner_lastLs(t, p); got != "gdrive:PAIM-Backup/f.bin" {
		t.Errorf("lsjson target = %q, want gdrive:PAIM-Backup/f.bin", got)
	}
}

func runner_lastLs(t *testing.T, p *Plugin) string {
	t.Helper()
	fr := p.runner.(*fakeRunner)
	call := fr.callWith("lsjson")
	if call == nil {
		t.Fatal("no lsjson call recorded")
	}
	return call[len(call)-1]
}

func TestVerifyHashMismatch(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f.bin")
	content := []byte("local content")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Same size, different md5.
	lsjson := []byte(fmt.Sprintf(`[{"Path":"f.bin","Name":"f.bin","Size":%d,"IsDir":false,"Hashes":{"md5":"%s"}}]`,
		len(content), md5Hex([]byte("XXXXX content"))))
	p := initedVerifyPlugin(t, verifyRunner([]string{"gdrive:"}, lsjson, nil, nil), "PAIM-Backup")

	ok, err := p.Verify(context.Background(), src, "f.bin")
	if err != nil {
		t.Fatalf("verify err: %v", err)
	}
	if ok {
		t.Fatalf("verify should fail on md5 mismatch, returning (false,nil)")
	}
}

func TestVerifySizeMismatch(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(src, []byte("1234567890"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	lsjson := []byte(`[{"Path":"f.bin","Name":"f.bin","Size":5,"IsDir":false,"Hashes":{"md5":"deadbeef"}}]`)
	p := initedVerifyPlugin(t, verifyRunner([]string{"gdrive:"}, lsjson, nil, nil), "PAIM-Backup")

	ok, err := p.Verify(context.Background(), src, "f.bin")
	if err != nil || ok {
		t.Fatalf("size mismatch should give (false,nil), got (%v,%v)", ok, err)
	}
}

func TestVerifyNoHashSizeOnly(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f.bin")
	content := []byte("sized only backend")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Backend returns no hashes at all.
	lsjson := []byte(fmt.Sprintf(`[{"Path":"f.bin","Name":"f.bin","Size":%d,"IsDir":false,"Hashes":{}}]`, len(content)))
	p := initedVerifyPlugin(t, verifyRunner([]string{"gdrive:"}, lsjson, nil, nil), "PAIM-Backup")

	ok, err := p.Verify(context.Background(), src, "f.bin")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatalf("size-only verify should pass when sizes match and no hash offered")
	}
}

func TestVerifyMissingRemote(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	p := initedVerifyPlugin(t, verifyRunner([]string{"gdrive:"}, nil, errors.New("exit status 1"), []byte("directory not found")), "PAIM-Backup")
	ok, err := p.Verify(context.Background(), src, "f.bin")
	if err != nil || ok {
		t.Fatalf("missing remote should give (false,nil), got (%v,%v)", ok, err)
	}
}

// ---- Authenticate error mapping ----

func TestAuthenticateSuccess(t *testing.T) {
	runner := &fakeRunner{responder: listRemotesResponder([]string{"gdrive:"}, func(ctx context.Context, args []string, onLine func(string)) ([]byte, []byte, error) {
		if args[0] == "about" {
			return []byte(`{"total":1,"used":0}`), nil, nil
		}
		return nil, nil, nil
	})}
	p := newTestPlugin(runner)
	if err := p.Initialize(context.Background(), `{"remote":"gdrive:"}`); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := p.Authenticate(context.Background()); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
}

func TestAuthenticateExpiredTokenMapsToReconnect(t *testing.T) {
	runner := &fakeRunner{responder: listRemotesResponder([]string{"gdrive:"}, func(ctx context.Context, args []string, onLine func(string)) ([]byte, []byte, error) {
		if args[0] == "about" {
			return nil, []byte("Failed to get about: couldn't fetch token: invalid_grant"), errors.New("exit status 1")
		}
		return nil, nil, nil
	})}
	p := newTestPlugin(runner)
	if err := p.Initialize(context.Background(), `{"remote":"gdrive:"}`); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	err := p.Authenticate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "reconnect gdrive:") {
		t.Fatalf("expected reconnect guidance, got %v", err)
	}
}

func TestAuthenticateAboutUnsupportedFallsBackToLsd(t *testing.T) {
	runner := &fakeRunner{responder: listRemotesResponder([]string{"sftp:"}, func(ctx context.Context, args []string, onLine func(string)) ([]byte, []byte, error) {
		switch args[0] {
		case "about":
			return nil, []byte("Error: about not supported by this remote"), errors.New("exit status 1")
		case "lsd":
			return []byte("          -1 2024-01-01 00:00:00        -1 dir\n"), nil, nil
		}
		return nil, nil, nil
	})}
	p := newTestPlugin(runner)
	if err := p.Initialize(context.Background(), `{"remote":"sftp:"}`); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := p.Authenticate(context.Background()); err != nil {
		t.Fatalf("authenticate via lsd fallback: %v", err)
	}
	if runner.callWith("lsd") == nil {
		t.Fatalf("expected lsd fallback to run")
	}
}

// ---- Delete ----

func TestDeleteUsesDeletefileAndIgnoresMissing(t *testing.T) {
	runner := &fakeRunner{responder: listRemotesResponder([]string{"gdrive:"}, func(ctx context.Context, args []string, onLine func(string)) ([]byte, []byte, error) {
		if args[0] == "deletefile" {
			// Simulate the object being already gone the second time.
			return nil, []byte("object not found"), errors.New("exit status 3")
		}
		return nil, nil, nil
	})}
	p := newTestPlugin(runner)
	if err := p.Initialize(context.Background(), `{"remote":"gdrive:","path":"PAIM-Backup"}`); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	// Missing object -> no error.
	if err := p.Delete(context.Background(), "2024/gone.jpg"); err != nil {
		t.Fatalf("delete missing should be nil, got %v", err)
	}
	call := runner.callWith("deletefile")
	if call == nil || call[len(call)-1] != "gdrive:PAIM-Backup/2024/gone.jpg" {
		t.Fatalf("deletefile target wrong: %v", call)
	}
}

func TestDeleteSurfacesRealError(t *testing.T) {
	runner := &fakeRunner{responder: listRemotesResponder([]string{"gdrive:"}, func(ctx context.Context, args []string, onLine func(string)) ([]byte, []byte, error) {
		if args[0] == "deletefile" {
			return nil, []byte("permission denied"), errors.New("exit status 1")
		}
		return nil, nil, nil
	})}
	p := newTestPlugin(runner)
	if err := p.Initialize(context.Background(), `{"remote":"gdrive:"}`); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := p.Delete(context.Background(), "x.jpg"); err == nil {
		t.Fatalf("expected a real delete error to surface")
	}
}

// ---- Cancellation kills the process ----

func TestUploadCancellationReturnsContextError(t *testing.T) {
	src := filepath.Join(t.TempDir(), "big.mov")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	started := make(chan struct{})
	runner := &fakeRunner{responder: listRemotesResponder([]string{"gdrive:"}, func(ctx context.Context, args []string, onLine func(string)) ([]byte, []byte, error) {
		if args[0] == "copyto" {
			close(started)
			<-ctx.Done() // real execRunner kills the process; the fake honors ctx.
			return nil, nil, ctx.Err()
		}
		return nil, nil, nil
	})}
	p := newTestPlugin(runner)
	if err := p.Initialize(context.Background(), `{"remote":"gdrive:"}`); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- p.Upload(ctx, src, "big.mov", nil) }()
	<-started
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upload did not return after cancellation")
	}
}

// ---- execRunner really kills a subprocess on cancel ----

func TestExecRunnerKillsProcessOnCancel(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, _, err := execRunner{}.Run(ctx, "sleep", []string{"30"}, nil)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
		if time.Since(start) > 5*time.Second {
			t.Fatalf("process not killed promptly")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("execRunner did not return after cancel; process not killed")
	}
}

// ---- Optional integration test against a real rclone remote ----

func TestIntegrationRealRclone(t *testing.T) {
	remote := os.Getenv("PAIM_RCLONE_TEST_REMOTE")
	if remote == "" {
		t.Skip("set PAIM_RCLONE_TEST_REMOTE (e.g. myremote:) to run the rclone integration test")
	}
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skip("rclone not installed")
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "paim-rclone-it.bin")
	content := []byte("PAIM rclone integration test payload " + time.Now().Format(time.RFC3339Nano))
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	p := New().(*Plugin)
	cfg := fmt.Sprintf(`{"remote":%q,"path":"PAIM-IntegrationTest"}`, remote)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := p.Initialize(ctx, cfg); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := p.Authenticate(ctx); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	rel := "it/" + filepath.Base(src)
	if err := p.Upload(ctx, src, rel, func(d, tot int64) {}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	ok, err := p.Verify(ctx, src, rel)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatalf("verify should pass for a just-uploaded file")
	}
	if err := p.Delete(ctx, rel); err != nil {
		t.Fatalf("delete: %v", err)
	}
	t.Logf("integration round-trip OK against %s", remote)
}
