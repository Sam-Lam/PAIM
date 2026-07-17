package metadata

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// readyMarker is the token exiftool writes to stdout after finishing a request
// in -stay_open mode. It always occupies its own line.
const readyMarker = "{ready}"

// exifTags is the fixed set of tags requested from exiftool. Restricting the
// output keeps batches small and deterministic (important for import throughput
// and for golden fixtures) while still reading maker notes — which is why -fast
// is deliberately not used. GPS reference tags are requested so the sign can be
// corrected defensively regardless of whether exiftool returns the raw EXIF
// (unsigned) or the composite (signed) coordinate.
var exifTags = []string{
	"-SubSecDateTimeOriginal",
	"-DateTimeOriginal",
	"-CreateDate",
	"-Make",
	"-Model",
	"-LensModel",
	"-LensID",
	"-Lens",
	"-ISO",
	"-ExposureTime",
	"-ShutterSpeed",
	"-FNumber",
	"-Aperture",
	"-GPSLatitude",
	"-GPSLatitudeRef",
	"-GPSLongitude",
	"-GPSLongitudeRef",
	"-ImageWidth",
	"-ImageHeight",
	"-ExifImageWidth",
	"-ExifImageHeight",
	"-Orientation",
	"-Duration",
	"-VideoFrameRate",
	"-CompressorID",
	"-CompressorName",
	"-ColorSpace",
	"-ContentIdentifier",
}

// buildArgs assembles the exiftool argument list (one token per stay_open line)
// for the given files. The -execute terminator is appended by the writer, not
// here.
func buildArgs(paths []string) []string {
	args := make([]string, 0, 6+len(exifTags)+len(paths))
	args = append(args,
		"-json",
		"-n",
		"-api", "largefilesupport=1",
		"-charset", "filename=utf8",
	)
	args = append(args, exifTags...)
	args = append(args, paths...)
	return args
}

// record is a single exiftool JSON object with lazily-typed field access. Values
// arrive as either JSON strings or JSON numbers depending on the tag and on -n,
// so every accessor tolerates both encodings.
type record map[string]json.RawMessage

// str returns the field as a string. Numbers are returned in their raw textual
// form; a missing field yields "".
func (r record) str(key string) string {
	raw, ok := r[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(string(raw))
}

// float returns the field as a float64, accepting both JSON numbers and numeric
// strings. The bool reports whether a usable value was present.
func (r record) float(key string) (float64, bool) {
	raw, ok := r[key]
	if !ok {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// intVal returns the field as an int (truncating any fraction). The bool reports
// whether a usable value was present.
func (r record) intVal(key string) (int, bool) {
	f, ok := r.float(key)
	if !ok {
		return 0, false
	}
	return int(f), true
}

// firstNonEmpty returns the first non-empty string among the given field keys.
func (r record) firstNonEmpty(keys ...string) string {
	for _, k := range keys {
		if v := r.str(k); v != "" {
			return v
		}
	}
	return ""
}

// gps reads a coordinate and its reference, returning signed decimal degrees.
// negChar is 'S' for latitude and 'W' for longitude: when the reference begins
// with that letter and the value is positive, the value is negated. This is a
// no-op when exiftool already returned a signed value.
func (r record) gps(valKey, refKey string, negChar byte) *float64 {
	f, ok := r.float(valKey)
	if !ok {
		return nil
	}
	ref := strings.ToUpper(r.str(refKey))
	if ref != "" && ref[0] == negChar && f > 0 {
		f = -f
	}
	return &f
}

// parseExifJSON parses the JSON array exiftool emits (with -json) into normalized
// AssetMetadata records. It is the entire, subprocess-independent parsing layer.
func parseExifJSON(data []byte) ([]*AssetMetadata, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, nil
	}
	var records []record
	if err := json.Unmarshal([]byte(trimmed), &records); err != nil {
		return nil, fmt.Errorf("decoding exiftool json: %w", err)
	}
	out := make([]*AssetMetadata, 0, len(records))
	for _, rec := range records {
		out = append(out, rec.toMetadata())
	}
	return out, nil
}

// toMetadata normalizes one exiftool record into an AssetMetadata.
func (r record) toMetadata() *AssetMetadata {
	m := &AssetMetadata{
		SourceFile:        r.str("SourceFile"),
		CameraMake:        r.str("Make"),
		CameraModel:       r.str("Model"),
		Lens:              r.firstNonEmpty("LensModel", "LensID", "Lens"),
		ContentIdentifier: r.str("ContentIdentifier"),
	}

	m.CaptureDate = r.captureDate()

	if iso, ok := r.intVal("ISO"); ok {
		m.ISO = iso
	}
	m.ShutterSpeed = formatShutterSpeed(r)
	m.Aperture = formatAperture(r)

	m.GPSLatitude = r.gps("GPSLatitude", "GPSLatitudeRef", 'S')
	m.GPSLongitude = r.gps("GPSLongitude", "GPSLongitudeRef", 'W')

	if o, ok := r.intVal("Orientation"); ok {
		m.Orientation = o
	}
	m.Width, m.Height = r.dimensions(m.Orientation)

	if d, ok := r.float("Duration"); ok {
		m.DurationSeconds = d
	}
	if fr, ok := r.float("VideoFrameRate"); ok {
		m.FrameRate = fr
	}
	m.Codec = r.firstNonEmpty("CompressorID", "CompressorName")
	m.ColorSpace = colorSpaceName(r.str("ColorSpace"))

	return m
}

// captureDate applies the SubSecDateTimeOriginal > DateTimeOriginal > CreateDate
// priority, returning the first value that parses to a valid time.
func (r record) captureDate() *time.Time {
	for _, key := range []string{"SubSecDateTimeOriginal", "DateTimeOriginal", "CreateDate"} {
		if t, ok := parseExifTime(r.str(key)); ok {
			return &t
		}
	}
	return nil
}

// dimensions resolves pixel dimensions, preferring the full-image tags and
// falling back to the EXIF-embedded ones, then swaps width/height when the EXIF
// orientation denotes a 90/270-degree rotation (values 5–8).
func (r record) dimensions(orientation int) (w, h int) {
	if v, ok := r.intVal("ImageWidth"); ok {
		w = v
	} else if v, ok := r.intVal("ExifImageWidth"); ok {
		w = v
	}
	if v, ok := r.intVal("ImageHeight"); ok {
		h = v
	} else if v, ok := r.intVal("ExifImageHeight"); ok {
		h = v
	}
	if orientation >= 5 && orientation <= 8 {
		w, h = h, w
	}
	return w, h
}

// exifTimeLayouts are tried in order. The first two consume an explicit timezone
// (Go's "Z07:00" accepts both "Z" and numeric offsets); the optional fractional
// ".999999999" matches values with or without sub-seconds.
var exifTimeLayouts = []string{
	"2006:01:02 15:04:05.999999999Z07:00",
	"2006:01:02 15:04:05Z07:00",
}

var exifNaiveLayouts = []string{
	"2006:01:02 15:04:05.999999999",
	"2006:01:02 15:04:05",
}

// parseExifTime parses an exiftool datetime. Timezone-bearing values keep their
// offset; naive values are interpreted in the local timezone. The exiftool
// "all zeros" placeholder and empty strings are treated as absent.
func parseExifTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "0000") {
		return time.Time{}, false
	}
	for _, layout := range exifTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	for _, layout := range exifNaiveLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// formatShutterSpeed reconstructs a human display string from the numeric
// exposure time (lost to -n). Values below one second become "1/N"; one second
// and above are rendered as a compact decimal. Returns "" when unavailable.
func formatShutterSpeed(r record) string {
	t, ok := r.float("ExposureTime")
	if !ok {
		t, ok = r.float("ShutterSpeed")
	}
	if !ok || t <= 0 {
		return ""
	}
	if t < 1 {
		denom := int(math.Round(1 / t))
		if denom <= 0 {
			return ""
		}
		return "1/" + strconv.Itoa(denom)
	}
	return strconv.FormatFloat(t, 'g', -1, 64)
}

// formatAperture reconstructs an "f/N" string from the numeric f-number.
func formatAperture(r record) string {
	f, ok := r.float("FNumber")
	if !ok {
		f, ok = r.float("Aperture")
	}
	if !ok || f <= 0 {
		return ""
	}
	return "f/" + strconv.FormatFloat(f, 'g', -1, 64)
}

// colorSpaceName maps the numeric EXIF ColorSpace value produced by -n to a
// readable name, passing through any value that is already textual.
func colorSpaceName(v string) string {
	switch v {
	case "":
		return ""
	case "1":
		return "sRGB"
	case "2":
		return "Adobe RGB"
	case "65535":
		return "Uncalibrated"
	default:
		return v
	}
}
