// Package version exposes the single, canonical PAIM application version. It is
// stamped into the library lock file, recorded in library_meta on every open,
// and surfaced in Settings → About. Keeping it in its own tiny leaf package (no
// internal imports) lets any layer reference it without creating a dependency
// cycle.
package version

// Version is the current PAIM application version (semantic versioning). Bump it
// when releasing; migrations and the library_meta bookkeeping key off it.
const Version = "0.1.0"
