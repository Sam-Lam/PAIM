// Package e2e contains PAIM's full-stack, cross-package scenario test and the
// shared fixture builders it relies on. The fixture code lives in this NON-test
// file (not *_test.go) on purpose: the standalone `gendata` and `populate`
// commands under internal/e2e import it to generate the very same dummy source
// trees the scenario test exercises, so the manual GUI dataset and the automated
// test are guaranteed to be built the same way.
//
// Nothing in this file imports the testing package; every builder returns errors
// so it is usable from an ordinary main(). The scenario test wraps these with
// t.Helper assertions.
package e2e

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// ExifSpec describes the EXIF tags to stamp into a JPEG via exiftool. A zero
// GPSLat/GPSLon (both nil) writes no GPS.
type ExifSpec struct {
	Date   time.Time
	Make   string
	Model  string
	GPSLat *float64
	GPSLon *float64
}

// FixtureFile is one media file the builder created, with the ground-truth facts
// the scenario test asserts against (expected capture date, camera, whether it is
// an exact-content duplicate, etc.).
type FixtureFile struct {
	Path        string    // absolute path on disk
	Ext         string    // lowercase, no leading dot
	CaptureDate time.Time // EXIF date for JPEGs; file mtime for RAW/video
	FromMtime   bool      // true when CaptureDate came from mtime (no EXIF)
	CameraMake  string
	CameraModel string
	HasGPS      bool
	IsDuplicate bool  // exact byte copy of an earlier media file in the tree
	Size        int64 // bytes on disk
}

// Tree is the manifest of a generated source tree: the media files (excluding
// junk) plus any junk/hidden files written for realism.
type Tree struct {
	Root      string
	Media     []FixtureFile
	JunkPaths []string
}

// MediaCount returns the number of non-junk media files in the tree.
func (t *Tree) MediaCount() int { return len(t.Media) }

// DuplicateCount returns how many media files are exact-content duplicates.
func (t *Tree) DuplicateCount() int {
	n := 0
	for _, f := range t.Media {
		if f.IsDuplicate {
			n++
		}
	}
	return n
}

// jpegBytes returns a deterministic minimal 1x1 JPEG. Determinism matters so a
// re-run of gendata/populate produces byte-identical files (hence identical
// content hashes -> "already imported" on a second import).
func jpegBytes(seed uint8) ([]byte, error) {
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: seed, G: seed / 2, B: seed / 3, A: 255})
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		return nil, fmt.Errorf("encode jpeg: %w", err)
	}
	return buf.Bytes(), nil
}

// writeJPEGWithEXIF writes a tiny valid JPEG at path and stamps EXIF via the
// exiftool binary bin.
func writeJPEGWithEXIF(bin, path string, seed uint8, spec ExifSpec) error {
	b, err := jpegBytes(seed)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write jpeg %q: %w", path, err)
	}
	return writeEXIF(bin, path, spec)
}

// writeEXIF stamps EXIF tags into an existing image using exiftool.
func writeEXIF(bin, path string, spec ExifSpec) error {
	dt := spec.Date.Format("2006:01:02 15:04:05")
	args := []string{
		"-overwrite_original", "-q", "-q",
		"-DateTimeOriginal=" + dt,
		"-CreateDate=" + dt,
	}
	if spec.Make != "" {
		args = append(args, "-Make="+spec.Make)
	}
	if spec.Model != "" {
		args = append(args, "-Model="+spec.Model)
	}
	if spec.GPSLat != nil && spec.GPSLon != nil {
		lat, latRef := *spec.GPSLat, "N"
		if lat < 0 {
			lat, latRef = -lat, "S"
		}
		lon, lonRef := *spec.GPSLon, "E"
		if lon < 0 {
			lon, lonRef = -lon, "W"
		}
		args = append(args,
			fmt.Sprintf("-GPSLatitude=%.6f", lat), "-GPSLatitudeRef="+latRef,
			fmt.Sprintf("-GPSLongitude=%.6f", lon), "-GPSLongitudeRef="+lonRef,
		)
	}
	args = append(args, path)
	cmd := exec.Command(bin, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("exiftool write %q: %w: %s", path, err, bytes.TrimSpace(out))
	}
	return nil
}

// writeBlob writes size bytes of deterministic pseudo-random content (seeded by
// seed) at path, then sets its mtime to modTime. Used for the fake RAW/video
// files that carry no EXIF, so their capture date must fall back to mtime.
func writeBlob(path string, size int, seed int64, modTime time.Time) error {
	r := rand.New(rand.NewSource(seed))
	buf := make([]byte, size)
	_, _ = r.Read(buf) // rand.Rand.Read never returns an error
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		return fmt.Errorf("write blob %q: %w", path, err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		return fmt.Errorf("set mtime %q: %w", path, err)
	}
	return nil
}

// copyExact copies src to dst byte-for-byte (an exact content duplicate).
func copyExact(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %q: %w", src, err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		return fmt.Errorf("write %q: %w", dst, err)
	}
	return nil
}

func sizeOf(path string) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}

func ptr(f float64) *float64 { return &f }

// local builds a local-time timestamp; exiftool stores naive DateTimeOriginal
// which the extractor parses back as local time, so fixtures use local too.
func local(y int, mo time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, mo, d, h, mi, s, 0, time.Local)
}

// BuildSourceTree creates the primary "SD card" fixture under root and returns a
// manifest. Layout:
//
//	IMG_0001.JPG   EXIF 2023-06-10 09:15  Canon EOS R5
//	IMG_0002.JPG   EXIF 2023-06-10 14:20  Canon EOS R5
//	IMG_0003.JPG   EXIF 2023-06-11 08:05  Apple iPhone 15 Pro (+GPS)
//	IMG_0004.JPG   EXIF 2023-06-11 16:45  Nikon Z8
//	IMG_0004.RAF   64 KiB random, no EXIF, mtime 2023-06-11 16:45:30 (mtime fallback;
//	               shares a basename with IMG_0004.JPG but RAW never Live-Photo pairs)
//	CLIP_0001.MOV  128 KiB random, no EXIF, mtime 2023-06-12 10:00 (mtime fallback)
//	sub/IMG_0001_copy.JPG  exact byte copy of IMG_0001.JPG (a duplicate)
//	.DS_Store      junk (hidden; must be ignored by the scan)
func BuildSourceTree(root, exiftoolBin string) (*Tree, error) {
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir source tree: %w", err)
	}
	tree := &Tree{Root: root}

	// Four EXIF-bearing JPEGs across two days.
	jpegs := []struct {
		name string
		seed uint8
		spec ExifSpec
		gps  bool
	}{
		{"IMG_0001.JPG", 40, ExifSpec{Date: local(2023, 6, 10, 9, 15, 0), Make: "Canon", Model: "EOS R5"}, false},
		{"IMG_0002.JPG", 80, ExifSpec{Date: local(2023, 6, 10, 14, 20, 0), Make: "Canon", Model: "EOS R5"}, false},
		{"IMG_0003.JPG", 120, ExifSpec{Date: local(2023, 6, 11, 8, 5, 0), Make: "Apple", Model: "iPhone 15 Pro", GPSLat: ptr(36.2704), GPSLon: ptr(-121.8081)}, true},
		{"IMG_0004.JPG", 160, ExifSpec{Date: local(2023, 6, 11, 16, 45, 0), Make: "Nikon", Model: "Z8"}, false},
	}
	for _, j := range jpegs {
		p := filepath.Join(root, j.name)
		if err := writeJPEGWithEXIF(exiftoolBin, p, j.seed, j.spec); err != nil {
			return nil, err
		}
		tree.Media = append(tree.Media, FixtureFile{
			Path: p, Ext: "jpg", CaptureDate: j.spec.Date,
			CameraMake: j.spec.Make, CameraModel: j.spec.Model, HasGPS: j.gps, Size: sizeOf(p),
		})
	}

	// Fake RAW: no EXIF -> capture date falls back to mtime.
	rafMtime := local(2023, 6, 11, 16, 45, 30)
	raf := filepath.Join(root, "IMG_0004.RAF")
	if err := writeBlob(raf, 64<<10, 0xBEEF01, rafMtime); err != nil {
		return nil, err
	}
	tree.Media = append(tree.Media, FixtureFile{
		Path: raf, Ext: "raf", CaptureDate: rafMtime, FromMtime: true, Size: sizeOf(raf),
	})

	// Fake video: no EXIF -> mtime fallback.
	movMtime := local(2023, 6, 12, 10, 0, 0)
	mov := filepath.Join(root, "CLIP_0001.MOV")
	if err := writeBlob(mov, 128<<10, 0xC0FFEE, movMtime); err != nil {
		return nil, err
	}
	tree.Media = append(tree.Media, FixtureFile{
		Path: mov, Ext: "mov", CaptureDate: movMtime, FromMtime: true, Size: sizeOf(mov),
	})

	// Exact duplicate of IMG_0001.JPG in a subfolder, under a different name.
	dup := filepath.Join(root, "sub", "IMG_0001_copy.JPG")
	if err := copyExact(filepath.Join(root, "IMG_0001.JPG"), dup); err != nil {
		return nil, err
	}
	tree.Media = append(tree.Media, FixtureFile{
		Path: dup, Ext: "jpg", CaptureDate: jpegs[0].spec.Date,
		CameraMake: "Canon", CameraModel: "EOS R5", IsDuplicate: true, Size: sizeOf(dup),
	})

	// Hidden junk that the scan must ignore.
	junk := filepath.Join(root, ".DS_Store")
	if err := os.WriteFile(junk, []byte("\x00\x01junk\x00"), 0o644); err != nil {
		return nil, fmt.Errorf("write junk: %w", err)
	}
	tree.JunkPaths = append(tree.JunkPaths, junk)

	return tree, nil
}

// BuildAdoptTree creates the "existing library on the archive drive" fixture used
// by the adopt-in-place scenario: two unique EXIF JPEGs plus a
// duplicate-of-each-other pair (DSC_0003.JPG and DSC_0003_dup.JPG).
func BuildAdoptTree(root, exiftoolBin string) (*Tree, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir adopt tree: %w", err)
	}
	tree := &Tree{Root: root}

	uniques := []struct {
		name string
		seed uint8
		spec ExifSpec
	}{
		{"DSC_0001.JPG", 200, ExifSpec{Date: local(2022, 9, 1, 12, 0, 0), Make: "Sony", Model: "ILCE-7M4"}},
		{"DSC_0002.JPG", 210, ExifSpec{Date: local(2022, 9, 2, 13, 30, 0), Make: "Sony", Model: "ILCE-7M4"}},
		{"DSC_0003.JPG", 220, ExifSpec{Date: local(2022, 9, 3, 14, 45, 0), Make: "Sony", Model: "ILCE-7M4"}},
	}
	for _, u := range uniques {
		p := filepath.Join(root, u.name)
		if err := writeJPEGWithEXIF(exiftoolBin, p, u.seed, u.spec); err != nil {
			return nil, err
		}
		tree.Media = append(tree.Media, FixtureFile{
			Path: p, Ext: "jpg", CaptureDate: u.spec.Date,
			CameraMake: u.spec.Make, CameraModel: u.spec.Model, Size: sizeOf(p),
		})
	}

	// The duplicate half of the pair: an exact copy of DSC_0003.JPG.
	dup := filepath.Join(root, "DSC_0003_dup.JPG")
	if err := copyExact(filepath.Join(root, "DSC_0003.JPG"), dup); err != nil {
		return nil, err
	}
	tree.Media = append(tree.Media, FixtureFile{
		Path: dup, Ext: "jpg", CaptureDate: uniques[2].spec.Date,
		CameraMake: "Sony", CameraModel: "ILCE-7M4", IsDuplicate: true, Size: sizeOf(dup),
	})

	return tree, nil
}

// BuildLargeDataset generates a richer dummy library for manual GUI testing: 15
// EXIF JPEGs spread across three dates and several cameras (two with GPS), two
// exact duplicates in a dupes/ subfolder, a RAW+JPEG pair, one video, and a
// hidden junk file. It reuses the same primitives as the scenario fixtures so the
// manual dataset matches what the test exercises.
func BuildLargeDataset(root, exiftoolBin string) (*Tree, error) {
	if err := os.MkdirAll(filepath.Join(root, "dupes"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir dataset: %w", err)
	}
	tree := &Tree{Root: root}

	dates := []time.Time{
		local(2024, 1, 5, 10, 0, 0),
		local(2024, 1, 6, 11, 0, 0),
		local(2024, 1, 7, 12, 0, 0),
	}
	cameras := []struct{ make_, model string }{
		{"Canon", "EOS R5"},
		{"Apple", "iPhone 15 Pro"},
		{"Nikon", "Z8"},
		{"Sony", "ILCE-7M4"},
		{"Fujifilm", "X-T5"},
	}

	var first, second string
	for i := 0; i < 15; i++ {
		name := fmt.Sprintf("IMG_%04d.JPG", 1000+i)
		p := filepath.Join(root, name)
		cam := cameras[i%len(cameras)]
		d := dates[i%len(dates)].Add(time.Duration(i) * 7 * time.Minute)
		spec := ExifSpec{Date: d, Make: cam.make_, Model: cam.model}
		if i == 2 { // Big Sur
			spec.GPSLat, spec.GPSLon = ptr(36.2704), ptr(-121.8081)
		}
		if i == 9 { // Golden Gate
			spec.GPSLat, spec.GPSLon = ptr(37.8199), ptr(-122.4783)
		}
		if err := writeJPEGWithEXIF(exiftoolBin, p, uint8(30+i*13), spec); err != nil {
			return nil, err
		}
		tree.Media = append(tree.Media, FixtureFile{
			Path: p, Ext: "jpg", CaptureDate: d, CameraMake: cam.make_, CameraModel: cam.model,
			HasGPS: spec.GPSLat != nil, Size: sizeOf(p),
		})
		if i == 0 {
			first = p
		}
		if i == 5 {
			second = p
		}
	}

	// Two exact duplicates in dupes/.
	for _, d := range []struct{ src, name string }{
		{first, "copy_of_IMG_1000.JPG"},
		{second, "copy_of_IMG_1005.JPG"},
	} {
		dst := filepath.Join(root, "dupes", d.name)
		if err := copyExact(d.src, dst); err != nil {
			return nil, err
		}
		tree.Media = append(tree.Media, FixtureFile{
			Path: dst, Ext: "jpg", IsDuplicate: true, Size: sizeOf(dst),
		})
	}

	// A RAW+JPEG pair sharing a basename (mtime fallback for the RAW).
	rawMtime := local(2024, 1, 7, 12, 30, 0)
	pairJPEG := filepath.Join(root, "IMG_2000.JPG")
	if err := writeJPEGWithEXIF(exiftoolBin, pairJPEG, 99, ExifSpec{Date: local(2024, 1, 7, 12, 30, 0), Make: "Fujifilm", Model: "X-T5"}); err != nil {
		return nil, err
	}
	tree.Media = append(tree.Media, FixtureFile{
		Path: pairJPEG, Ext: "jpg", CaptureDate: local(2024, 1, 7, 12, 30, 0),
		CameraMake: "Fujifilm", CameraModel: "X-T5", Size: sizeOf(pairJPEG),
	})
	pairRAW := filepath.Join(root, "IMG_2000.RAF")
	if err := writeBlob(pairRAW, 96<<10, 0x2000, rawMtime); err != nil {
		return nil, err
	}
	tree.Media = append(tree.Media, FixtureFile{
		Path: pairRAW, Ext: "raf", CaptureDate: rawMtime, FromMtime: true, Size: sizeOf(pairRAW),
	})

	// One video.
	vidMtime := local(2024, 1, 6, 18, 0, 0)
	vid := filepath.Join(root, "MOV_3000.MOV")
	if err := writeBlob(vid, 160<<10, 0x3000, vidMtime); err != nil {
		return nil, err
	}
	tree.Media = append(tree.Media, FixtureFile{
		Path: vid, Ext: "mov", CaptureDate: vidMtime, FromMtime: true, Size: sizeOf(vid),
	})

	// Hidden junk.
	junk := filepath.Join(root, ".DS_Store")
	if err := os.WriteFile(junk, []byte("\x00junk"), 0o644); err != nil {
		return nil, fmt.Errorf("write junk: %w", err)
	}
	tree.JunkPaths = append(tree.JunkPaths, junk)

	return tree, nil
}
