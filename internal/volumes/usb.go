package volumes

import (
	"encoding/json"
	"fmt"
	"strings"
)

// usbNode is one entry in the nested `_items` tree of `system_profiler
// SPUSBDataType -json` (and, with the same shape, SPCardReaderDataType). A
// storage device carries a Media array whose entries hold the BSD disk name we
// correlate against a mounted volume's whole-disk node.
type usbNode struct {
	Name         string     `json:"_name"`
	BSDName      string     `json:"bsd_name"`
	Manufacturer string     `json:"manufacturer"`
	VendorID     string     `json:"vendor_id"`
	ProductID    string     `json:"product_id"`
	SerialNum    string     `json:"serial_num"`
	Media        []usbMedia `json:"Media"`
	Items        []usbNode  `json:"_items"`
}

// usbMedia is a storage medium exposed by a USB/card device. Its bsd_name is
// the whole-disk node (e.g. "disk4"); nested volumes carry the slice nodes.
type usbMedia struct {
	Name    string      `json:"_name"`
	BSDName string      `json:"bsd_name"`
	Volumes []usbVolume `json:"volumes"`
}

type usbVolume struct {
	Name    string `json:"_name"`
	BSDName string `json:"bsd_name"`
}

// usbDeviceInfo is the hardware identity extracted for a matched device.
type usbDeviceInfo struct {
	Serial       string
	Manufacturer string
	Model        string
	VendorID     string
	ProductID    string
}

// parseUSBData walks system_profiler JSON under topKey looking for the device
// backing wholeDisk (e.g. "disk4"). It returns the matched device's identity
// and true, or a zero value and false when no node references that disk.
func parseUSBData(topKey string, raw []byte, wholeDisk string) (*usbDeviceInfo, bool, error) {
	wholeDisk = normaliseBSD(wholeDisk)
	if wholeDisk == "" {
		return nil, false, nil
	}
	var root map[string][]usbNode
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, false, fmt.Errorf("volumes: parse %s json: %w", topKey, err)
	}
	nodes, ok := root[topKey]
	if !ok {
		return nil, false, nil
	}
	for i := range nodes {
		if dev := findUSBDevice(&nodes[i], wholeDisk); dev != nil {
			return deviceIdentity(dev), true, nil
		}
	}
	return nil, false, nil
}

// findUSBDevice returns the node whose Media (or own BSD name) references
// wholeDisk, searching depth-first through the _items tree.
func findUSBDevice(node *usbNode, wholeDisk string) *usbNode {
	if nodeReferencesDisk(node, wholeDisk) {
		return node
	}
	for i := range node.Items {
		if dev := findUSBDevice(&node.Items[i], wholeDisk); dev != nil {
			return dev
		}
	}
	return nil
}

// nodeReferencesDisk reports whether a node is (or contains media that is) the
// given whole disk. A leaf-slice match (e.g. disk4s1) still resolves to the
// owning device because we compare against the normalised whole-disk name.
func nodeReferencesDisk(node *usbNode, wholeDisk string) bool {
	if normaliseBSD(node.BSDName) == wholeDisk {
		return true
	}
	for _, m := range node.Media {
		if normaliseBSD(m.BSDName) == wholeDisk {
			return true
		}
		for _, v := range m.Volumes {
			if normaliseBSD(v.BSDName) == wholeDisk {
				return true
			}
		}
	}
	return false
}

func deviceIdentity(node *usbNode) *usbDeviceInfo {
	return &usbDeviceInfo{
		Serial:       strings.TrimSpace(node.SerialNum),
		Manufacturer: strings.TrimSpace(firstNonEmpty(node.Manufacturer, vendorName(node.VendorID))),
		Model:        strings.TrimSpace(node.Name),
		VendorID:     extractHexID(node.VendorID),
		ProductID:    extractHexID(node.ProductID),
	}
}

// normaliseBSD reduces a device path or slice node to its whole-disk name:
// "/dev/disk4s1" -> "disk4", "disk4s1" -> "disk4", "disk4" -> "disk4".
func normaliseBSD(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "/dev/")
	if !strings.HasPrefix(s, "disk") {
		return s
	}
	rest := s[len("disk"):]
	// Keep leading digits (the whole-disk number); drop any sNN partition tail.
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return s
	}
	return "disk" + rest[:end]
}

// extractHexID pulls the leading hex token from a system_profiler id string
// such as "0x04e8  (Samsung Electronics Co., Ltd.)".
func extractHexID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i]
	}
	return s
}

// vendorName extracts the parenthesised manufacturer name from a vendor_id
// string when a dedicated manufacturer field is absent.
func vendorName(vendorID string) string {
	open := strings.IndexByte(vendorID, '(')
	closeIdx := strings.LastIndexByte(vendorID, ')')
	if open >= 0 && closeIdx > open {
		return strings.TrimSpace(vendorID[open+1 : closeIdx])
	}
	return ""
}
