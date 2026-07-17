package volumes

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestParseDiskutilInfo_SDCard(t *testing.T) {
	info, err := parseDiskutilInfo("/Volumes/EOS_DIGITAL", readFixture(t, "diskutil_sdcard.plist"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.VolumeUUID != "0F8A6C21-D3E4-4F5A-9B7C-1122AABBCCDD" {
		t.Errorf("VolumeUUID = %q", info.VolumeUUID)
	}
	if info.FilesystemUUID != "4A1B2C3D-1111-2222-3333-444455556666" {
		t.Errorf("FilesystemUUID (PartitionUUID) = %q", info.FilesystemUUID)
	}
	if info.FilesystemType != "exfat" {
		t.Errorf("FilesystemType = %q", info.FilesystemType)
	}
	if info.ConnectionType != ConnectionSDXC {
		t.Errorf("ConnectionType = %q, want sdxc", info.ConnectionType)
	}
	if !info.Removable || !info.Ejectable || info.Internal {
		t.Errorf("removable/ejectable/internal = %v/%v/%v", info.Removable, info.Ejectable, info.Internal)
	}
	if info.CapacityBytes != 63999836160 {
		t.Errorf("CapacityBytes = %d", info.CapacityBytes)
	}
	if info.FreeBytes != 51000000000 {
		t.Errorf("FreeBytes = %d", info.FreeBytes)
	}
	if info.WholeDiskDeviceNode != "disk5" {
		t.Errorf("WholeDiskDeviceNode = %q", info.WholeDiskDeviceNode)
	}
	if info.SectorSize != 512 {
		t.Errorf("SectorSize = %d", info.SectorSize)
	}
	if info.VolumeName != "EOS_DIGITAL" {
		t.Errorf("VolumeName = %q", info.VolumeName)
	}
}

func TestParseDiskutilInfo_USBSSD(t *testing.T) {
	info, err := parseDiskutilInfo("/Volumes/T7", readFixture(t, "diskutil_usb_ssd.plist"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.ConnectionType != ConnectionUSB {
		t.Errorf("ConnectionType = %q, want usb", info.ConnectionType)
	}
	if info.FilesystemUUID != "AB12CD34-EF56-7890-ABCD-1234567890EF" {
		t.Errorf("FilesystemUUID (DiskUUID) = %q", info.FilesystemUUID)
	}
	if info.FilesystemType != "apfs" {
		t.Errorf("FilesystemType = %q", info.FilesystemType)
	}
	if info.CapacityBytes != 1000555581440 {
		t.Errorf("CapacityBytes = %d", info.CapacityBytes)
	}
	if info.Model != "Samsung PSSD T7 Media" {
		t.Errorf("Model = %q", info.Model)
	}
}

func TestParseDiskutilInfo_APFSInternal(t *testing.T) {
	info, err := parseDiskutilInfo("/System/Volumes/Data", readFixture(t, "diskutil_apfs_internal.plist"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.ConnectionType != ConnectionInternal {
		t.Errorf("ConnectionType = %q, want internal", info.ConnectionType)
	}
	if !info.Internal {
		t.Errorf("Internal = false, want true")
	}
	if info.Removable {
		t.Errorf("Removable = true, want false")
	}
	// FreeSpace is 0, so APFSContainerFree must be used.
	if info.FreeBytes != 57007001600 {
		t.Errorf("FreeBytes = %d, want APFSContainerFree fallback", info.FreeBytes)
	}
}

func TestParseDiskutilInfo_SMB(t *testing.T) {
	info, err := parseDiskutilInfo("/Volumes/photos", readFixture(t, "diskutil_smb.plist"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.FilesystemType != "smbfs" {
		t.Errorf("FilesystemType = %q", info.FilesystemType)
	}
	// classifyConnection alone (without the network flag) cannot tell smbfs is a
	// network volume; Describe promotes it. Here the raw classification is unknown.
	if info.ConnectionType != ConnectionUnknown {
		t.Errorf("ConnectionType = %q, want unknown (pre network-promotion)", info.ConnectionType)
	}
}

func TestClassifyConnection(t *testing.T) {
	cases := []struct {
		bus      string
		internal bool
		network  bool
		want     ConnectionType
	}{
		{"USB", false, false, ConnectionUSB},
		{"usb", false, false, ConnectionUSB},
		{"Secure Digital", false, false, ConnectionSDXC},
		{"Thunderbolt", false, false, ConnectionThunderbolt},
		{"Apple Fabric", true, false, ConnectionInternal},
		{"PCI-Express", false, false, ConnectionInternal},
		{"SATA", false, false, ConnectionInternal},
		{"", true, false, ConnectionInternal},
		{"", false, false, ConnectionUnknown},
		{"USB", false, true, ConnectionNetwork},
		{"Secure Digital", false, true, ConnectionNetwork},
	}
	for _, c := range cases {
		if got := classifyConnection(c.bus, c.internal, c.network); got != c.want {
			t.Errorf("classifyConnection(%q, internal=%v, net=%v) = %q, want %q", c.bus, c.internal, c.network, got, c.want)
		}
	}
}

func TestParseUSBData_MatchSSD(t *testing.T) {
	dev, found, err := parseUSBData("SPUSBDataType", readFixture(t, "spusb.json"), "disk4")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !found {
		t.Fatal("expected to find disk4")
	}
	if dev.Serial != "S5R9NS0T123456X" {
		t.Errorf("Serial = %q", dev.Serial)
	}
	if dev.Manufacturer != "Samsung" {
		t.Errorf("Manufacturer = %q", dev.Manufacturer)
	}
	if dev.Model != "Portable SSD T7" {
		t.Errorf("Model = %q", dev.Model)
	}
	if dev.VendorID != "0x04e8" {
		t.Errorf("VendorID = %q", dev.VendorID)
	}
	if dev.ProductID != "0x4001" {
		t.Errorf("ProductID = %q", dev.ProductID)
	}
}

func TestParseUSBData_MatchBySliceNode(t *testing.T) {
	// Passing a slice node normalises to the whole disk before matching.
	dev, found, err := parseUSBData("SPUSBDataType", readFixture(t, "spusb.json"), "/dev/disk6s1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !found {
		t.Fatal("expected to find disk6 via slice node")
	}
	if dev.Serial != "000000264001" {
		t.Errorf("Serial = %q", dev.Serial)
	}
	if dev.Manufacturer != "Generic" {
		t.Errorf("Manufacturer = %q", dev.Manufacturer)
	}
}

func TestParseUSBData_NoMatch(t *testing.T) {
	_, found, err := parseUSBData("SPUSBDataType", readFixture(t, "spusb.json"), "disk99")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if found {
		t.Error("expected no match for disk99")
	}
}

func TestParseCardReaderData(t *testing.T) {
	dev, found, err := parseUSBData("SPCardReaderDataType", readFixture(t, "spcardreader.json"), "disk5")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !found {
		t.Fatal("expected to find disk5 card reader")
	}
	if dev.Serial != "SDCARD-CANON-987654" {
		t.Errorf("Serial = %q", dev.Serial)
	}
}

func TestNormaliseBSD(t *testing.T) {
	cases := map[string]string{
		"/dev/disk4s1": "disk4",
		"disk4s1":      "disk4",
		"disk4":        "disk4",
		"/dev/disk12s3s1": "disk12",
		"":             "",
	}
	for in, want := range cases {
		if got := normaliseBSD(in); got != want {
			t.Errorf("normaliseBSD(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLookupMount_SMB(t *testing.T) {
	out := `//evan@nas.local/photos on /Volumes/photos (smbfs, nodev, nosuid, mounted by evan)
/dev/disk4s1 on /Volumes/T7 (apfs, local, nodev, nosuid, journaled)`
	dev, fs, found := lookupMount(out, "/Volumes/photos")
	if !found {
		t.Fatal("expected to find /Volumes/photos")
	}
	if dev != "//evan@nas.local/photos" {
		t.Errorf("device = %q", dev)
	}
	if fs != "smbfs" {
		t.Errorf("fs = %q", fs)
	}
}

// TestDescribe_NetworkVolume drives Describe end-to-end with a fake runner that
// returns the SMB diskutil fixture and a matching mount table, verifying network
// promotion and that hardware probing is skipped.
func TestDescribe_NetworkVolume(t *testing.T) {
	mountOut := "//evan@nas.local/photos on /Volumes/photos (smbfs, nodev, nosuid)\n"
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "diskutil":
			return readFixture(t, "diskutil_smb.plist"), nil
		case "mount":
			return []byte(mountOut), nil
		default:
			t.Fatalf("unexpected command %s: hardware probe should be skipped for network volumes", name)
			return nil, nil
		}
	}
	c := NewCollector(nil, WithRunner(runner))
	info, err := c.Describe(context.Background(), "/Volumes/photos")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !info.IsNetworkVolume {
		t.Error("IsNetworkVolume = false, want true")
	}
	if info.NetworkURL != "//evan@nas.local/photos" {
		t.Errorf("NetworkURL = %q", info.NetworkURL)
	}
	if info.ConnectionType != ConnectionNetwork {
		t.Errorf("ConnectionType = %q, want network", info.ConnectionType)
	}
}

// TestDescribe_USBHardwareProbe drives Describe with fixtures for a USB SSD and
// checks that hardware identity is merged from system_profiler.
func TestDescribe_USBHardwareProbe(t *testing.T) {
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "diskutil":
			return readFixture(t, "diskutil_usb_ssd.plist"), nil
		case "mount":
			return []byte("/dev/disk4s1 on /Volumes/T7 (apfs, local)\n"), nil
		case "system_profiler":
			if len(args) > 0 && args[0] == "SPUSBDataType" {
				return readFixture(t, "spusb.json"), nil
			}
			return []byte(`{"SPCardReaderDataType":[]}`), nil
		default:
			return nil, nil
		}
	}
	c := NewCollector(nil, WithRunner(runner))
	info, err := c.Describe(context.Background(), "/Volumes/T7")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if info.IsNetworkVolume {
		t.Error("IsNetworkVolume = true, want false")
	}
	if info.HardwareSerial != "S5R9NS0T123456X" {
		t.Errorf("HardwareSerial = %q", info.HardwareSerial)
	}
	if info.Manufacturer != "Samsung" {
		t.Errorf("Manufacturer = %q", info.Manufacturer)
	}
	if info.USBVendorID != "0x04e8" {
		t.Errorf("USBVendorID = %q", info.USBVendorID)
	}
}

// TestDescribe_ProbeFailureDegrades ensures a failed hardware probe adds a
// warning rather than failing Describe.
func TestDescribe_ProbeFailureDegrades(t *testing.T) {
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "diskutil":
			return readFixture(t, "diskutil_usb_ssd.plist"), nil
		case "mount":
			return []byte(""), nil
		case "system_profiler":
			return nil, context.DeadlineExceeded
		default:
			return nil, nil
		}
	}
	c := NewCollector(nil, WithRunner(runner))
	info, err := c.Describe(context.Background(), "/Volumes/T7")
	if err != nil {
		t.Fatalf("Describe should degrade, not fail: %v", err)
	}
	if info.HardwareSerial != "" {
		t.Errorf("HardwareSerial = %q, want empty", info.HardwareSerial)
	}
	if len(info.Warnings) == 0 {
		t.Error("expected warnings for failed probes")
	}
}
