// Package volumes enumerates and describes mounted macOS volumes and watches
// /Volumes for mount/unmount events. It gathers the hardware and filesystem
// facts PAIM uses to identify an import source (via internal/source): hardware
// serial, filesystem and volume UUIDs, capacity, connection type, and so on.
//
// Facts are collected from `diskutil info -plist <mount>` and, for USB and SD
// devices, `system_profiler SPUSBDataType -json` / `SPCardReaderDataType`.
// Network volumes (smbfs, afpfs, nfs, webdav) are recognised from their
// filesystem type and the `mount` table; hardware probing is skipped for them.
//
// Collection degrades gracefully: a failed or missing probe never fails the
// whole description. Missing fields stay at their zero value and the gap is
// noted in Info.Warnings so callers can explain reduced confidence.
package volumes

// ConnectionType classifies how a volume's device is attached to the machine.
// It is derived from diskutil's BusProtocol plus network/internal signals and
// is used (together with media heuristics) to infer a domain.SourceType.
type ConnectionType string

// ConnectionType values. Unknown is used when the bus protocol cannot be
// mapped confidently.
const (
	ConnectionUSB         ConnectionType = "usb"
	ConnectionThunderbolt ConnectionType = "thunderbolt"
	ConnectionSDXC        ConnectionType = "sdxc"
	ConnectionNetwork     ConnectionType = "network"
	ConnectionInternal    ConnectionType = "internal"
	ConnectionUnknown     ConnectionType = "unknown"
)

// Info is the collected description of a single mounted volume. Every field is
// best-effort: a zero value means "not determined" (see Warnings for why).
type Info struct {
	// MountPoint is the absolute path the volume is mounted at (e.g. /Volumes/SD).
	MountPoint string `json:"mountPoint"`
	// VolumeName is the user-visible label. It is stored for display only and is
	// NEVER used as identity (labels such as UNTITLED/NO NAME/EOS_DIGITAL repeat
	// across unrelated media).
	VolumeName string `json:"volumeName"`

	// VolumeUUID is a strong per-volume identifier (diskutil VolumeUUID).
	VolumeUUID string `json:"volumeUuid"`
	// FilesystemUUID is the partition/disk UUID (diskutil DiskUUID/PartitionUUID).
	FilesystemUUID string `json:"filesystemUuid"`
	// FilesystemType is the lowercase filesystem identifier (apfs, exfat, msdos,
	// hfs, smbfs, nfs, ...).
	FilesystemType string `json:"filesystemType"`

	CapacityBytes int64 `json:"capacityBytes"`
	FreeBytes     int64 `json:"freeBytes"`

	// DeviceNode is the mounted device (e.g. /dev/disk4s1).
	DeviceNode string `json:"deviceNode"`
	// WholeDiskDeviceNode is the parent whole-disk BSD node (e.g. disk4), used to
	// correlate against system_profiler entries.
	WholeDiskDeviceNode string `json:"wholeDiskDeviceNode"`

	Removable bool `json:"removable"`
	Internal  bool `json:"internal"`
	Ejectable bool `json:"ejectable"`

	ConnectionType ConnectionType `json:"connectionType"`

	// Hardware identity (from system_profiler, when available).
	HardwareSerial string `json:"hardwareSerial"`
	Manufacturer   string `json:"manufacturer"`
	Model          string `json:"model"`

	SectorSize    int    `json:"sectorSize"`
	USBVendorID   string `json:"usbVendorId"`
	USBProductID  string `json:"usbProductId"`

	// Network volume fields.
	IsNetworkVolume bool   `json:"isNetworkVolume"`
	NetworkURL      string `json:"networkUrl"`

	// Warnings records probes that failed or fields that could not be determined.
	// Their presence explains why identification confidence may be reduced; they
	// never by themselves indicate an error.
	Warnings []string `json:"warnings,omitempty"`
}

// EventType distinguishes a mount from an unmount.
type EventType string

// EventType values.
const (
	EventMounted   EventType = "mounted"
	EventUnmounted EventType = "unmounted"
)

// Event is emitted by a Watcher when a volume appears or disappears under the
// watched directory.
type Event struct {
	MountPoint string    `json:"mountPoint"`
	Type       EventType `json:"type"`
}
