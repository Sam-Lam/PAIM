package rclone

import (
	"context"
	"fmt"
	"strings"
)

// hashlessBackends are rclone backend types that do not expose a usable content
// hash, so PAIM cannot verify an upload to them byte-for-byte. Google Photos is
// the motivating case (the Photos API returns no checksum). Uploads to these are
// only sensible as MIRROR (quality-of-life) destinations; the setup UI pre-checks
// the Mirror toggle and warns that uploads cannot be verified. The list is a
// conservative denylist — any backend NOT listed is assumed checksum-capable, and
// the manual Mirror toggle is always the fallback if that assumption is wrong.
var hashlessBackends = map[string]bool{
	"googlephotos": true,
}

// throttledBackends are backend types that need conservative rclone transfer
// flags (low transactions-per-second, single transfer) to stay within tight
// per-second write limits. Google Photos is the motivating case.
var throttledBackends = map[string]bool{
	"googlephotos": true,
}

// backendSupportsChecksum reports whether a backend of the given type exposes a
// content hash PAIM can verify against. Unknown/empty types are treated as
// checksum-capable (optimistic; the Mirror toggle is the manual fallback).
func backendSupportsChecksum(backendType string) bool {
	return !hashlessBackends[strings.ToLower(strings.TrimSpace(backendType))]
}

// backendNeedsThrottle reports whether uploads to a backend of the given type
// should pass conservative rclone flags (--tpslimit / single transfer).
func backendNeedsThrottle(backendType string) bool {
	return throttledBackends[strings.ToLower(strings.TrimSpace(backendType))]
}

// backendIsGooglePhotos reports whether a backend type is rclone's Google Photos
// backend, whose virtual filesystem only accepts uploads under album/<name>/... or
// upload/... roots (see remotePathFor's gphotos mapping).
func backendIsGooglePhotos(backendType string) bool {
	return strings.ToLower(strings.TrimSpace(backendType)) == "googlephotos"
}

// detectBackendType returns the rclone backend type of a remote (e.g. "drive",
// "googlephotos", "s3") by parsing `rclone config show <remote>`. It returns an
// empty type without error when the type line is absent (a best-effort probe:
// callers degrade to the optimistic default).
func detectBackendType(ctx context.Context, runner commandRunner, binary, remote string) (string, error) {
	name := strings.TrimRight(strings.TrimSpace(remote), ":")
	stdout, stderr, err := runner.Run(ctx, binary, []string{"config", "show", name}, nil)
	if err != nil {
		return "", fmt.Errorf("rclone: config show %q: %w: %s", name, err, lastLine(stderr))
	}
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(key) == "type" {
			return strings.TrimSpace(value), nil
		}
	}
	return "", nil
}

// RemoteInfo reports a remote's backend type and whether PAIM can verify uploads
// to it (i.e. the backend exposes a content hash). It is exported for the setup
// UI, which uses it to pre-check the Mirror toggle and warn when uploads to the
// chosen remote cannot be verified. binary may be empty to auto-discover rclone.
func RemoteInfo(ctx context.Context, binary, remote string) (backendType string, supportsChecksum bool, err error) {
	resolved, err := ResolveBinary(binary)
	if err != nil {
		return "", false, err
	}
	backendType, err = detectBackendType(ctx, execRunner{}, resolved, remote)
	if err != nil {
		return "", false, err
	}
	return backendType, backendSupportsChecksum(backendType), nil
}
