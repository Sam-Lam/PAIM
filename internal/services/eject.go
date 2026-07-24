package services

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Eject errors surfaced across the binding boundary. The frontend distinguishes
// them to word the disabled-button tooltip / toast.
var (
	// ErrEjectEmptyMount is returned for an empty mount point.
	ErrEjectEmptyMount = errors.New("services: eject: empty mount point")
	// ErrEjectLibraryVolume is returned when the target volume holds the open
	// library root — ejecting it would pull the catalog out from under PAIM.
	ErrEjectLibraryVolume = errors.New("services: eject: refusing to eject the volume the open library lives on")
)

// ejectRunner runs `diskutil eject <mountPoint>` and returns its combined
// output. It is a package var so tests substitute it and never eject anything
// real on the developer's machine.
var ejectRunner = func(ctx context.Context, mountPoint string) ([]byte, error) {
	return exec.CommandContext(ctx, "diskutil", "eject", mountPoint).CombinedOutput()
}

// ejectActivity is the minimal view of the activity tracker EjectVolume consults
// to refuse ejecting a volume a live operation is still touching. *ActivityTracker
// satisfies it.
type ejectActivity interface {
	ActivePaths() []string
}

// SetActivity injects the shared activity tracker so EjectVolume can refuse
// ejecting a volume an in-flight operation is using. Called once from main.go.
func (s *SourcesService) SetActivity(a ejectActivity) { s.activity = a }

// EjectVolume ejects the volume mounted at mountPoint via `diskutil eject`, with
// two server-side safety guards it NEVER delegates to the frontend:
//
//  1. it refuses to eject the volume the open library lives on
//     (ErrEjectLibraryVolume); and
//  2. it refuses when any tracked long operation (import/analyze/reorganize/
//     safe-to-erase/clear/cleanup) is currently touching a path on that volume,
//     naming the reason.
//
// The OS is the final backstop: `diskutil eject` itself fails (surfaced here as a
// readable message) if the volume has open files, so a case the path guards do
// not cover — e.g. a backup writing to a destination on this volume — still can
// never eject a busy disk. On success the volume unmounts and the existing
// volume:unmounted watcher event refreshes the UI. Logged under the source
// subsystem.
func (s *SourcesService) EjectVolume(ctx context.Context, mountPoint string) error {
	if err := s.guard(); err != nil {
		return err
	}
	mp := normalizePath(mountPoint)
	if mp == "" {
		return ErrEjectEmptyMount
	}
	// Resolve the actual volume mount point containing the given path (best-effort):
	// the clear-source flow may pass a source subfolder rather than the bare mount,
	// and diskutil eject needs the volume. A miss or no collector leaves mp as-is.
	if vol := s.resolveMountPoint(ctx, mp); vol != "" {
		mp = vol
	}
	// Guard 1: never eject the library's own volume.
	if s.root != "" && volumeContainsPath(mp, s.root) {
		return ErrEjectLibraryVolume
	}
	// Guard 2: never eject a volume a live operation is touching.
	if s.activity != nil {
		for _, p := range s.activity.ActivePaths() {
			if volumeContainsPath(mp, p) {
				return fmt.Errorf("services: eject: an operation is still using this volume (%s) — wait for it to finish or cancel it first", p)
			}
		}
	}

	ejCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := ejectRunner(ejCtx, mp)
	if err != nil {
		return fmt.Errorf("services: eject %q: %s: %w", mp, diskutilMessage(out), err)
	}
	s.log.Info("volume ejected", "mountPoint", mp)
	return nil
}

// resolveMountPoint returns the mount point of the volume that contains path —
// the longest volume mount that is a prefix of path — or "" when the collector
// is unset, enumeration fails, or nothing matches (path used as-is then).
func (s *SourcesService) resolveMountPoint(ctx context.Context, path string) string {
	if s.collector == nil {
		return ""
	}
	vols, err := s.collector.List(ctx)
	if err != nil {
		return ""
	}
	best := ""
	for _, v := range vols {
		mp := normalizePath(v.MountPoint)
		if mp == "" {
			continue
		}
		if (path == mp || volumeContainsPath(mp, path)) && len(mp) > len(best) {
			best = mp
		}
	}
	return best
}

// diskutilMessage extracts a readable one-line message from diskutil's combined
// output (its errors are usually a single sentence like "Disk … could not be
// unmounted because it is in use"). Empty output yields a generic fallback.
func diskutilMessage(out []byte) string {
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "diskutil eject failed"
	}
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = strings.TrimSpace(text[:i])
	}
	return text
}
