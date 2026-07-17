package source

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/autolinepro/paim/internal/domain"
	"github.com/autolinepro/paim/internal/volumes"
)

// fakeDescriber returns a fixed Info.
type fakeDescriber struct {
	info *volumes.Info
	err  error
}

func (d fakeDescriber) Describe(_ context.Context, _ string) (*volumes.Info, error) {
	return d.info, d.err
}

// fakeStore is an in-memory SourceStore keyed by the strong identifiers.
type fakeStore struct {
	bySerial map[string]*domain.ImportSource
	byVolume map[string]*domain.ImportSource
	byFS     map[string]*domain.ImportSource
	recent   []domain.ImportSource
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		bySerial: map[string]*domain.ImportSource{},
		byVolume: map[string]*domain.ImportSource{},
		byFS:     map[string]*domain.ImportSource{},
	}
}

func (s *fakeStore) FindByHardwareSerial(_ context.Context, v string) (*domain.ImportSource, error) {
	return s.bySerial[v], nil
}
func (s *fakeStore) FindByVolumeUUID(_ context.Context, v string) (*domain.ImportSource, error) {
	return s.byVolume[v], nil
}
func (s *fakeStore) FindByFilesystemUUID(_ context.Context, v string) (*domain.ImportSource, error) {
	return s.byFS[v], nil
}
func (s *fakeStore) ListRecent(_ context.Context, _ int) ([]domain.ImportSource, error) {
	return s.recent, nil
}

func TestInferSourceType(t *testing.T) {
	cases := []struct {
		name string
		info volumes.Info
		want domain.SourceType
	}{
		{"sd card", volumes.Info{ConnectionType: volumes.ConnectionSDXC}, domain.SourceTypeSDCard},
		{"usb ssd", volumes.Info{ConnectionType: volumes.ConnectionUSB, Model: "Samsung PSSD T7", Removable: true}, domain.SourceTypeUSBSSD},
		{"usb hdd by keyword", volumes.Info{ConnectionType: volumes.ConnectionUSB, Model: "My Passport 25E2"}, domain.SourceTypeExternalHDD},
		{"thunderbolt ssd", volumes.Info{ConnectionType: volumes.ConnectionThunderbolt, Model: "SanDisk Pro-G40 SSD"}, domain.SourceTypeUSBSSD},
		{"internal", volumes.Info{ConnectionType: volumes.ConnectionInternal}, domain.SourceTypeInternalFolder},
		{"smb network", volumes.Info{IsNetworkVolume: true, ConnectionType: volumes.ConnectionNetwork, FilesystemType: "smbfs"}, domain.SourceTypeSMBShare},
		{"nfs network", volumes.Info{IsNetworkVolume: true, ConnectionType: volumes.ConnectionNetwork, FilesystemType: "nfs"}, domain.SourceTypeNASFolder},
		{"unknown removable", volumes.Info{ConnectionType: volumes.ConnectionUnknown, Removable: true}, domain.SourceTypeUSBSSD},
		{"unknown fixed", volumes.Info{ConnectionType: volumes.ConnectionUnknown}, domain.SourceTypeInternalFolder},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := inferSourceType(&c.info); got != c.want {
				t.Errorf("inferSourceType = %q, want %q", got, c.want)
			}
		})
	}
}

func TestIdentify_ConfidenceScoring(t *testing.T) {
	known := func(mut func(*domain.ImportSource)) *domain.ImportSource {
		s := &domain.ImportSource{
			UUIDModel:      domain.UUIDModel{ID: "src-known"},
			VolumeLabel:    "EOS_DIGITAL",
			CapacityBytes:  64_000_000_000,
			FilesystemType: "exfat",
			LastSeenAt:     time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		}
		if mut != nil {
			mut(s)
		}
		return s
	}

	cases := []struct {
		name        string
		info        volumes.Info
		setup       func(*fakeStore)
		wantConf    int
		wantKnown   bool
		reasonMatch string
	}{
		{
			name: "hardware serial match",
			info: volumes.Info{HardwareSerial: "ABC123", ConnectionType: volumes.ConnectionSDXC},
			setup: func(s *fakeStore) {
				s.bySerial["ABC123"] = known(func(k *domain.ImportSource) { k.HardwareSerial = "ABC123" })
			},
			wantConf:    scoreHardwareSerial,
			wantKnown:   true,
			reasonMatch: "Hardware serial ABC123",
		},
		{
			name: "volume uuid match",
			info: volumes.Info{VolumeUUID: "VOL-1", ConnectionType: volumes.ConnectionUSB},
			setup: func(s *fakeStore) {
				s.byVolume["VOL-1"] = known(func(k *domain.ImportSource) { k.VolumeUUID = "VOL-1" })
			},
			wantConf:    scoreVolumeUUID,
			wantKnown:   true,
			reasonMatch: "Volume UUID VOL-1",
		},
		{
			name: "fs uuid + capacity + type match",
			info: volumes.Info{FilesystemUUID: "FS-1", CapacityBytes: 64_000_000_000, FilesystemType: "exfat", ConnectionType: volumes.ConnectionSDXC},
			setup: func(s *fakeStore) {
				s.byFS["FS-1"] = known(func(k *domain.ImportSource) { k.FilesystemUUID = "FS-1" })
			},
			wantConf:    scoreFSFull,
			wantKnown:   true,
			reasonMatch: "capacity, and filesystem type all match",
		},
		{
			name: "fs uuid match but capacity differs",
			info: volumes.Info{FilesystemUUID: "FS-1", CapacityBytes: 128_000_000_000, FilesystemType: "exfat", ConnectionType: volumes.ConnectionSDXC},
			setup: func(s *fakeStore) {
				s.byFS["FS-1"] = known(func(k *domain.ImportSource) { k.FilesystemUUID = "FS-1" })
			},
			wantConf:    scoreFSCapacityDiffer,
			wantKnown:   true,
			reasonMatch: "capacity differs",
		},
		{
			name: "fs uuid match but fs type differs",
			info: volumes.Info{FilesystemUUID: "FS-1", CapacityBytes: 64_000_000_000, FilesystemType: "apfs", ConnectionType: volumes.ConnectionSDXC},
			setup: func(s *fakeStore) {
				s.byFS["FS-1"] = known(func(k *domain.ImportSource) { k.FilesystemUUID = "FS-1" })
			},
			wantConf:    scoreFSTypeDiffer,
			wantKnown:   true,
			reasonMatch: "filesystem type differs",
		},
		{
			name:        "no match, new source",
			info:        volumes.Info{HardwareSerial: "NOPE", ConnectionType: volumes.ConnectionSDXC},
			setup:       func(s *fakeStore) {},
			wantConf:    scoreNone,
			wantKnown:   false,
			reasonMatch: "new source",
		},
		{
			name: "strongest signal wins over weaker",
			info: volumes.Info{
				HardwareSerial: "ABC123",
				FilesystemUUID: "FS-1", CapacityBytes: 999, FilesystemType: "apfs",
				ConnectionType: volumes.ConnectionSDXC,
			},
			setup: func(s *fakeStore) {
				s.bySerial["ABC123"] = known(func(k *domain.ImportSource) { k.HardwareSerial = "ABC123"; k.ID = "hw" })
				s.byFS["FS-1"] = known(func(k *domain.ImportSource) { k.FilesystemUUID = "FS-1"; k.ID = "fs" })
			},
			wantConf:    scoreHardwareSerial,
			wantKnown:   true,
			reasonMatch: "Hardware serial",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newFakeStore()
			c.setup(store)
			id := NewIdentifier(fakeDescriber{info: &c.info}, store, nil, nil) // hasher/isMedia nil: skip fingerprint
			m, err := id.Identify(context.Background(), "/Volumes/X")
			if err != nil {
				t.Fatalf("Identify: %v", err)
			}
			if m.Confidence != c.wantConf {
				t.Errorf("Confidence = %d, want %d (reasons: %v)", m.Confidence, c.wantConf, m.Reasons)
			}
			if m.IsKnown != c.wantKnown {
				t.Errorf("IsKnown = %v, want %v", m.IsKnown, c.wantKnown)
			}
			joined := strings.Join(m.Reasons, " ")
			if !strings.Contains(joined, c.reasonMatch) {
				t.Errorf("reasons %q do not contain %q", joined, c.reasonMatch)
			}
			if m.SourceRecord == nil {
				t.Fatal("SourceRecord is nil")
			}
			if m.SourceRecord.ConfidenceReason == "" {
				t.Error("SourceRecord.ConfidenceReason is empty")
			}
		})
	}
}

// TestIdentify_ContentFingerprintMatch verifies that a matching stored content
// hash marks contents previously imported and scores 100 even without a hardware
// or UUID match.
func TestIdentify_ContentFingerprintMatch(t *testing.T) {
	root := buildTree(t)

	// Precompute this tree's fingerprint to seed a "known" source.
	fp, err := ComputeFingerprint(context.Background(), root, fakeHasher{}, isMediaTest, nil)
	if err != nil {
		t.Fatal(err)
	}

	store := newFakeStore()
	store.recent = []domain.ImportSource{{
		UUIDModel:          domain.UUIDModel{ID: "prev"},
		VolumeLabel:        "OldCard",
		ContentFingerprint: fp.JSON(),
	}}

	info := &volumes.Info{ConnectionType: volumes.ConnectionSDXC} // no hardware/UUID identity
	id := NewIdentifier(fakeDescriber{info: info}, store, fakeHasher{}, isMediaTest)

	// Identify uses the mount root as the fingerprint source; point it at root.
	m, err := id.Identify(context.Background(), root)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if !m.ContentsPreviouslyImported {
		t.Error("ContentsPreviouslyImported = false, want true")
	}
	if m.Confidence != scoreContentHash {
		t.Errorf("Confidence = %d, want %d", m.Confidence, scoreContentHash)
	}
	if !strings.Contains(strings.Join(m.Reasons, " "), "previously imported") {
		t.Errorf("missing content-match reason: %v", m.Reasons)
	}
}

// TestIdentify_PathOnlyMatch verifies a same-layout / different-contents match
// scores lower and does not mark contents previously imported.
func TestIdentify_PathOnlyMatch(t *testing.T) {
	root := buildTree(t)
	fp, err := ComputeFingerprint(context.Background(), root, fakeHasher{}, isMediaTest, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Same path hash, different content hash.
	stored := Fingerprint{PathHash: fp.PathHash, ContentHash: "different"}

	store := newFakeStore()
	store.recent = []domain.ImportSource{{
		UUIDModel:          domain.UUIDModel{ID: "prev"},
		VolumeLabel:        "OldCard",
		ContentFingerprint: stored.JSON(),
	}}

	info := &volumes.Info{ConnectionType: volumes.ConnectionSDXC}
	id := NewIdentifier(fakeDescriber{info: info}, store, fakeHasher{}, isMediaTest)
	m, err := id.Identify(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if m.ContentsPreviouslyImported {
		t.Error("ContentsPreviouslyImported = true, want false for path-only match")
	}
	if m.Confidence != scorePathOnly {
		t.Errorf("Confidence = %d, want %d", m.Confidence, scorePathOnly)
	}
}

func TestIdentify_CandidateFieldsAndLabelNotIdentity(t *testing.T) {
	info := &volumes.Info{
		VolumeName:     "NO NAME",
		HardwareSerial: "S1",
		VolumeUUID:     "V1",
		FilesystemUUID: "F1",
		FilesystemType: "exfat",
		Manufacturer:   "Sony",
		Model:          "Tough SD",
		CapacityBytes:  128_000_000_000,
		ConnectionType: volumes.ConnectionSDXC,
	}
	store := newFakeStore() // nothing known
	id := NewIdentifier(fakeDescriber{info: info}, store, nil, nil)
	m, err := id.Identify(context.Background(), "/Volumes/NO NAME")
	if err != nil {
		t.Fatal(err)
	}
	rec := m.SourceRecord
	if rec.VolumeLabel != "NO NAME" {
		t.Errorf("label not stored for display: %q", rec.VolumeLabel)
	}
	if rec.SourceType != domain.SourceTypeSDCard {
		t.Errorf("SourceType = %q", rec.SourceType)
	}
	if rec.HardwareSerial != "S1" || rec.VolumeUUID != "V1" || rec.FilesystemUUID != "F1" {
		t.Errorf("identity fields not copied: %+v", rec)
	}
	if rec.ConnectionType != string(volumes.ConnectionSDXC) {
		t.Errorf("ConnectionType = %q", rec.ConnectionType)
	}
}
