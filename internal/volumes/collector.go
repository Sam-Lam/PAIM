package volumes

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// networkFilesystems are the filesystem types treated as network volumes: for
// these PAIM skips USB/hardware probing and records a NetworkURL instead.
var networkFilesystems = map[string]bool{
	"smbfs":  true,
	"afpfs":  true,
	"nfs":    true,
	"webdav": true,
	"ftp":    true,
}

// runnerFunc executes a command and returns its stdout. It is injected so tests
// can supply recorded fixtures instead of invoking real subprocesses.
type runnerFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// Collector describes and enumerates mounted volumes. It shells out to
// diskutil, system_profiler and mount; all calls are context-bounded and
// failures degrade gracefully into Info.Warnings.
type Collector struct {
	log        *slog.Logger
	run        runnerFunc
	volumesDir string
	cmdTimeout time.Duration
}

// Option configures a Collector.
type Option func(*Collector)

// WithVolumesDir overrides the directory scanned by List (default /Volumes).
func WithVolumesDir(dir string) Option { return func(c *Collector) { c.volumesDir = dir } }

// WithRunner overrides subprocess execution (used in tests).
func WithRunner(r runnerFunc) Option { return func(c *Collector) { c.run = r } }

// WithCommandTimeout overrides the per-subprocess timeout (default 10s).
func WithCommandTimeout(d time.Duration) Option { return func(c *Collector) { c.cmdTimeout = d } }

// NewCollector constructs a Collector. A nil logger falls back to the default
// slog logger tagged with the volumes subsystem.
func NewCollector(log *slog.Logger, opts ...Option) *Collector {
	c := &Collector{
		log:        log,
		volumesDir: "/Volumes",
		cmdTimeout: 10 * time.Second,
	}
	if c.log == nil {
		c.log = slog.Default().With(slog.String("subsystem", "volumes"))
	}
	c.run = c.execRunner
	for _, o := range opts {
		o(c)
	}
	return c
}

// execRunner is the default runner: it runs name with a per-call timeout
// derived from ctx and returns stdout. Stderr and linker (`ld: warning`) noise
// are ignored.
func (c *Collector) execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, c.cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("volumes: run %s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

// Describe collects everything PAIM knows about the volume mounted at
// mountPoint. It never returns an error merely because an optional probe
// failed: such gaps are recorded in the returned Info.Warnings. An error is
// returned only when the volume cannot be described at all (diskutil failed and
// no network fallback applied).
func (c *Collector) Describe(ctx context.Context, mountPoint string) (*Info, error) {
	raw, duErr := c.run(ctx, "diskutil", "info", "-plist", mountPoint)

	var info *Info
	if duErr == nil {
		parsed, perr := parseDiskutilInfo(mountPoint, raw)
		if perr != nil {
			return nil, perr
		}
		info = parsed
	} else {
		// diskutil can fail on some network mounts; fall back to a bare Info and
		// rely on the mount table for filesystem type / URL.
		info = &Info{MountPoint: mountPoint}
		info.Warnings = append(info.Warnings, fmt.Sprintf("diskutil info failed: %v", duErr))
	}

	// Network detection: from filesystem type or the mount table.
	if net, url := c.networkInfo(ctx, mountPoint, info.FilesystemType); net {
		info.IsNetworkVolume = true
		info.NetworkURL = url
		info.ConnectionType = ConnectionNetwork
		if info.FilesystemType == "" {
			info.Warnings = append(info.Warnings, "network volume: filesystem type unknown")
		}
		return info, nil
	}

	// Hardware probing for physical devices attached over USB or via a card
	// reader. Best-effort: absence of a match only adds a warning.
	c.probeHardware(ctx, info)
	return info, nil
}

// probeHardware augments info with serial/manufacturer/model/vendor/product ids
// from system_profiler, matched by whole-disk BSD node. Failures are warnings.
func (c *Collector) probeHardware(ctx context.Context, info *Info) {
	whole := normaliseBSD(firstNonEmpty(info.WholeDiskDeviceNode, info.DeviceNode))
	if whole == "" {
		info.Warnings = append(info.Warnings, "no whole-disk node: skipped hardware probe")
		return
	}

	// SD/card readers are described by SPCardReaderDataType; USB SSDs/HDDs and
	// USB card readers by SPUSBDataType. Try whichever is most relevant first,
	// then fall back to the other, so a USB-attached SD reader is still matched.
	order := []struct {
		key string
		arg string
	}{
		{"SPUSBDataType", "SPUSBDataType"},
		{"SPCardReaderDataType", "SPCardReaderDataType"},
	}
	if info.ConnectionType == ConnectionSDXC {
		order[0], order[1] = order[1], order[0]
	}

	for _, src := range order {
		raw, err := c.run(ctx, "system_profiler", src.arg, "-json")
		if err != nil {
			info.Warnings = append(info.Warnings, fmt.Sprintf("%s probe failed: %v", src.key, err))
			continue
		}
		dev, found, perr := parseUSBData(src.key, raw, whole)
		if perr != nil {
			info.Warnings = append(info.Warnings, fmt.Sprintf("%s parse failed: %v", src.key, perr))
			continue
		}
		if found {
			applyDeviceInfo(info, dev)
			return
		}
	}
	info.Warnings = append(info.Warnings, fmt.Sprintf("no system_profiler entry matched %s: hardware serial unavailable", whole))
}

// applyDeviceInfo copies non-empty hardware identity onto info without
// clobbering fields diskutil already provided.
func applyDeviceInfo(info *Info, dev *usbDeviceInfo) {
	if dev.Serial != "" {
		info.HardwareSerial = dev.Serial
	}
	if dev.Manufacturer != "" {
		info.Manufacturer = dev.Manufacturer
	}
	if dev.Model != "" {
		info.Model = dev.Model
	}
	if dev.VendorID != "" {
		info.USBVendorID = dev.VendorID
	}
	if dev.ProductID != "" {
		info.USBProductID = dev.ProductID
	}
}

// networkInfo reports whether mountPoint is a network volume and, if so, its
// backing URL. It uses the known filesystem type first, then consults the mount
// table (which also yields the //user@host/share URL for SMB/AFP/NFS).
func (c *Collector) networkInfo(ctx context.Context, mountPoint, fsType string) (bool, string) {
	url := ""
	isNet := networkFilesystems[fsType]

	raw, err := c.run(ctx, "mount", "")
	if err == nil {
		if dev, mfs, found := lookupMount(string(raw), mountPoint); found {
			if networkFilesystems[mfs] {
				isNet = true
			}
			if isNet {
				url = dev
			}
		}
	}
	return isNet, url
}

// lookupMount finds the entry for mountPoint in `mount` output, returning the
// device/URL, the filesystem type, and whether it was found. Lines look like:
//
//	//user@host/share on /Volumes/share (smbfs, nodev, nosuid, ...)
//	/dev/disk4s1 on /Volumes/SD (exfat, local, nodev, ...)
func lookupMount(mountOutput, mountPoint string) (device, fsType string, found bool) {
	for _, line := range strings.Split(mountOutput, "\n") {
		idx := strings.Index(line, " on ")
		if idx < 0 {
			continue
		}
		dev := strings.TrimSpace(line[:idx])
		rest := line[idx+len(" on "):]
		paren := strings.LastIndex(rest, " (")
		if paren < 0 {
			continue
		}
		mp := strings.TrimSpace(rest[:paren])
		if mp != mountPoint {
			continue
		}
		opts := strings.Trim(rest[paren+2:], "()")
		fs := opts
		if comma := strings.IndexByte(opts, ','); comma >= 0 {
			fs = opts[:comma]
		}
		return dev, strings.ToLower(strings.TrimSpace(fs)), true
	}
	return "", "", false
}

// List enumerates mountable volumes under the collector's volumes directory
// (default /Volumes), describing each. The root filesystem is excluded; hidden
// entries, the .timemachine automount, and Time Machine snapshot mounts are
// skipped. A single volume that fails to describe is logged and omitted rather
// than failing the whole enumeration.
func (c *Collector) List(ctx context.Context) ([]Info, error) {
	entries, err := os.ReadDir(c.volumesDir)
	if err != nil {
		return nil, fmt.Errorf("volumes: read %q: %w", c.volumesDir, err)
	}

	var out []Info
	for _, e := range entries {
		name := e.Name()
		if skipVolumeEntry(name) {
			continue
		}
		mount := filepath.Join(c.volumesDir, name)
		// Exclude anything resolving to the root filesystem (e.g. the
		// "Macintosh HD" symlink to /).
		if resolvesToRoot(mount) {
			continue
		}
		info, derr := c.Describe(ctx, mount)
		if derr != nil {
			c.log.WarnContext(ctx, "skip volume: describe failed", slog.String("mount", mount), slog.Any("err", derr))
			continue
		}
		out = append(out, *info)
	}
	return out, nil
}

// skipVolumeEntry reports whether a /Volumes entry name should be ignored.
func skipVolumeEntry(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") {
		return true
	}
	if name == ".timemachine" || strings.HasSuffix(name, ".timemachine") {
		return true
	}
	return false
}

// resolvesToRoot reports whether mount is a symlink (or path) resolving to "/".
func resolvesToRoot(mount string) bool {
	resolved, err := filepath.EvalSymlinks(mount)
	if err != nil {
		return false
	}
	return resolved == "/"
}
