package volumes

import (
	"fmt"
	"strings"

	"howett.net/plist"
)

// diskutilInfo mirrors the subset of `diskutil info -plist <mount>` output PAIM
// needs. Fields absent for a given volume decode to their zero value.
type diskutilInfo struct {
	VolumeUUID    string `plist:"VolumeUUID"`
	DiskUUID      string `plist:"DiskUUID"`
	PartitionUUID string `plist:"PartitionUUID"`

	VolumeName             string `plist:"VolumeName"`
	FilesystemType         string `plist:"FilesystemType"`
	FilesystemName         string `plist:"FilesystemName"`
	FilesystemUserVisible  string `plist:"FilesystemUserVisibleName"`

	TotalSize         int64 `plist:"TotalSize"`
	Size              int64 `plist:"Size"`
	FreeSpace         int64 `plist:"FreeSpace"`
	APFSContainerFree int64 `plist:"APFSContainerFree"`

	DeviceNode      string `plist:"DeviceNode"`
	DeviceIdentifier string `plist:"DeviceIdentifier"`
	ParentWholeDisk string `plist:"ParentWholeDisk"`

	RemovableMedia bool `plist:"RemovableMedia"`
	Removable      bool `plist:"Removable"`
	Internal       bool `plist:"Internal"`
	Ejectable      bool `plist:"Ejectable"`

	BusProtocol         string `plist:"BusProtocol"`
	IORegistryEntryName string `plist:"IORegistryEntryName"`
	MediaName           string `plist:"MediaName"`
	DeviceBlockSize     int    `plist:"DeviceBlockSize"`

	MountPoint string `plist:"MountPoint"`
}

// parseDiskutilInfo decodes the raw plist bytes returned by `diskutil info
// -plist` into a normalised Info. It fills every field it can and records
// nothing in Warnings itself (network detection and hardware probing add
// warnings at the Describe level).
func parseDiskutilInfo(mountPoint string, raw []byte) (*Info, error) {
	var d diskutilInfo
	if _, err := plist.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("volumes: parse diskutil plist for %q: %w", mountPoint, err)
	}

	info := &Info{
		MountPoint:          firstNonEmpty(d.MountPoint, mountPoint),
		VolumeName:          d.VolumeName,
		VolumeUUID:          d.VolumeUUID,
		FilesystemUUID:      firstNonEmpty(d.DiskUUID, d.PartitionUUID),
		FilesystemType:      strings.ToLower(firstNonEmpty(d.FilesystemType, d.FilesystemName)),
		CapacityBytes:       firstPositive(d.TotalSize, d.Size),
		FreeBytes:           firstPositive(d.FreeSpace, d.APFSContainerFree),
		DeviceNode:          d.DeviceNode,
		WholeDiskDeviceNode: d.ParentWholeDisk,
		Removable:           d.Removable || d.RemovableMedia,
		Internal:            d.Internal,
		Ejectable:           d.Ejectable,
		SectorSize:          d.DeviceBlockSize,
		Model:               firstNonEmpty(d.MediaName, d.IORegistryEntryName),
	}
	info.ConnectionType = classifyConnection(d.BusProtocol, info.Internal, false)
	return info, nil
}

// classifyConnection maps diskutil's BusProtocol (plus network/internal
// signals) to a ConnectionType. Matching is case-insensitive and tolerant of
// the varied strings diskutil emits ("USB", "Secure Digital", "Thunderbolt",
// "PCI-Express", "Apple Fabric", ...).
func classifyConnection(busProtocol string, internal, isNetwork bool) ConnectionType {
	if isNetwork {
		return ConnectionNetwork
	}
	b := strings.ToLower(busProtocol)
	switch {
	case strings.Contains(b, "secure digital"), strings.Contains(b, "sd card"), b == "sd":
		return ConnectionSDXC
	case strings.Contains(b, "thunderbolt"):
		return ConnectionThunderbolt
	case strings.Contains(b, "usb"):
		return ConnectionUSB
	case strings.Contains(b, "apple fabric"), strings.Contains(b, "pci"), strings.Contains(b, "sata"), strings.Contains(b, "ata"), strings.Contains(b, "nvme"):
		return ConnectionInternal
	case internal:
		return ConnectionInternal
	default:
		return ConnectionUnknown
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstPositive(vals ...int64) int64 {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}
