// Package version exposes the single, canonical PAIM application version. It is
// stamped into the library lock file, recorded in library_meta on every open,
// logged at startup, and surfaced in Settings → About and on the Welcome screen.
// Keeping it in its own tiny leaf package (no internal imports) lets any layer
// reference it without creating a dependency cycle.
package version

import "fmt"

// Version is the current PAIM application version (semantic versioning). Bump it
// when releasing; migrations and the library_meta bookkeeping key off it (the
// semver alone is what is stored in library_meta).
const Version = "0.4.1"

// Commit and Date are stamped at build time via -ldflags -X (see the darwin
// build task). They are empty in a plain `go build` / `go run` / test binary, in
// which case Full() falls back gracefully to a "dev" marker.
var (
	// Commit is the short git SHA the binary was built from ("" when unset).
	Commit string
	// Date is the build date (YYYY-MM-DD) ("" when unset).
	Date string
)

// Full returns a human-friendly build string combining the semantic version with
// the build commit and date, e.g. "0.2.0 (abc1234, 2026-07-22)". When the build
// stamps are absent (a dev build) it degrades to "0.2.0 (dev)"; a partially
// stamped build fills the missing half with "dev" so the shape is stable.
func Full() string {
	c, d := Commit, Date
	if c == "" && d == "" {
		return Version + " (dev)"
	}
	if c == "" {
		c = "dev"
	}
	if d == "" {
		d = "dev"
	}
	return fmt.Sprintf("%s (%s, %s)", Version, c, d)
}
