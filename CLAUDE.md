# PAIM — Photo Archive Integrity Manager

macOS desktop app (Wails v3 + Go backend + React/TS frontend) for photo/video import,
archive verification, backup, and storage reclamation.

**Read `docs/ARCHITECTURE.md` before writing any code.** It is the single source of truth
for package layout, database schema, hashing strategy, and all conventions. Do not deviate.

## Build & test

- Go: `go build ./...` (root), tests: `go test ./...`
- Frontend: `cd frontend && npm run build` (tsc + vite)
- Regenerate Wails bindings after changing service method signatures:
  `wails3 generate bindings` (wails3 is in `~/go/bin`)
- exiftool is installed at `/opt/homebrew/bin/exiftool`

## Hard rules

- Never risk data loss. Copy → fsync → verify (BLAKE3) → atomic rename → then record.
- Never mark success before verification. Never hard-delete DB rows (soft delete only).
- Never identify assets by filename/timestamp/EXIF — hashes only.
- Never identify sources by volume label — hardware IDs + content fingerprint + confidence.
- All long-running work: context cancellation + progress reporting.
