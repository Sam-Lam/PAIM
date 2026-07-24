# PAIM Roadmap

Current release: **v0.5.0** (see git tags for full release history; every release is
tagged and race-tested). This file records agreed-but-unbuilt work so any future
session can pick up without prior conversation context. Keep it updated as items ship.

## Shipped in v0.5.0 (2026-07-24) — Batches C & D complete

- **Batch C — duplicate triage at scale**: checkbox multi-select + "Resolve selected"
  as a cancellable, re-attachable background job (one typed confirm per batch);
  folder/session grouping + size sort with "select all in filter" via server-side
  ID lists; true total wasted bytes from SQL aggregates; live refresh on
  import-completed. Events: `duplicates:progress` / `duplicates:completed`.
- **Batch D — workflow loop-closers**: backup rolling jobs/min + bytes-remaining +
  "done ~Thursday" ETA on Backup Queue header and Dashboard (suppressed while
  paused/yielding); Cancel-all-pending/paused; macOS dock progress badge (Wails
  dock service, activity-tracker driven); eject affordance with server-side
  library-volume + active-operation guards and post-clear reminder; structured
  per-file import-failure records with Retry/Dismiss (**schema migration 4** —
  `import_failures` table; legacy sessions keep log-only view); Settings
  "this Mac"/"this library" + applies-immediately/restart labeling.

## Next up — approved, specced, unbuilt

(nothing queued — pick from the audit leftovers below with the user)

## Deferred by explicit decision — do not build without direction
- **Hot catalog (SSD working copy)**: see ARCHITECTURE.md "DEFERRED" section — split-brain
  reconciliation risk not yet justified; revisit if browsing still lags after thumbnail
  warm-up + SSD thumb cache (both shipped).
- **Self-contained .app distribution**: FUTURE SPEC in ARCHITECTURE.md — bundling
  strategy per dependency (librclone embed, bundled exiftool + perl risk). The
  `internal/toolpath` bundled-first lookup convention from that spec is NOT yet
  implemented — adopt it when convenient.

## Smaller audit leftovers (unranked, not yet approved individually)
- Sidebar count badges (pending imports / failed backups / duplicates)
- Terminology unification across pages (verified/complete/protected/archived)
- Context-menu discoverability hint; global keyboard shortcuts
- Onboarding checklist card for first-run (providers nudge shipped in v0.4.0)

## Operating conventions (for future sessions)
- Every release: full gauntlet (`go build ./...`, `go vet`, `go test -count=1 -race
  ./internal/...`, bindings regen when service surface changes, `npx tsc --noEmit`,
  `npm run build`), bump `internal/version.Version`, tag `vX.Y.Z`, push commit + tag.
- `scripts/update.sh` = user's pull-and-rebuild path on their machines.
- The user's LIBRARY lives on their other machine + external HDD (easystore). This dev
  machine has only the repo — never assume access to real data; diagnose from
  screenshots and code.
- Wails v3 pinned alpha2.117; wails3 CLI in ~/go/bin (PATH prefix needed for build
  subtasks). Frontend bindings arrays are `T[] | null` — always null-guard (`?? []`).
- Schema changes go through internal/db migrations (LatestSchemaVersion currently 4);
  additive columns via AutoMigrate + migration registration; pre-migration backups are
  automatic.
