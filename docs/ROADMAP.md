# PAIM Roadmap

Current release: **v0.4.2** (see git tags for full release history; every release is
tagged and race-tested). This file records agreed-but-unbuilt work so any future
session can pick up without prior conversation context. Keep it updated as items ship.

## Next up — approved, specced, unbuilt

### Batch C — duplicate triage at scale (from the QoL audit)
The Duplicate Manager is one-pair-at-a-time with a typed confirm per delete;
clearing the user's ~11k flagged pairs would take ~22,000 interactions.
- Checkbox multi-select with "Resolve selected" (one confirm per batch, typed word
  once per batch, not per pair)
- Grouping/filters: by folder, by session, by size; "select all in folder/session"
- True total wasted bytes from the service (today the header sums only the visible page)
- Optional: live refresh on import-completed

### Batch D — workflow loop-closers (from the QoL audit + user requests)
- Backup rate/ETA/last-completed on Backup Queue header + Dashboard ("11,402 pending"
  → "done ~Thursday"); rolling completed-jobs/min + bytes-remaining
- Eject affordance: button on volume cards + clear-source completion (diskutil eject),
  soft reminder when a cleared removable is still mounted
- Import failed-files panel: structured per-session failure records (path, op, error)
  with per-file Retry and **Dismiss** (resolve "file vanished before import" cases);
  legacy sessions keep log-only view
- Cancel all pending/paused backups (bulk mirror of Retry-all-failed)
- macOS dock progress/badge during long imports/backfills/warm-ups
- Settings polish: per-section "applies immediately" vs header-save labeling,
  restart-required notes, "this Mac" vs "this library" tags per section

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
- Duplicates page live-refresh on background imports
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
- Schema changes go through internal/db migrations (LatestSchemaVersion currently 3);
  additive columns via AutoMigrate + migration registration; pre-migration backups are
  automatic.
