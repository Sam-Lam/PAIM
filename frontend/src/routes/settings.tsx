import { useEffect, useState } from "react";
import {
  ArrowPathIcon,
  ArrowsRightLeftIcon,
  BoltIcon,
  CameraIcon,
  CheckCircleIcon,
  CircleStackIcon,
  ExclamationTriangleIcon,
  FolderArrowDownIcon,
  FolderOpenIcon,
  InformationCircleIcon,
  PhotoIcon,
  StopIcon,
  TrashIcon,
} from "@heroicons/react/24/outline";
import { Button, Card, ConfirmDialog, LoadingBlock, PageHeader, ProgressBar, StatusBadge } from "../components";
import {
  AppService,
  BackupService,
  ImportService,
  LibraryService,
  SettingsService,
  Settings,
  SnapshotService,
  ThumbnailService,
  WailsEvents,
  type ImportCompleted,
  type ImportProgress,
  type RecentLibraryDTO,
  type ReorganizePlanDTO,
  type ReorganizePlanProgress,
  type SnapshotStatusDTO,
  type ThumbCacheDTO,
  type ThumbsProgress,
  type WarmupStatusDTO,
} from "../lib/api";
import { useWailsEvent } from "../lib/hooks";
import { useLibrary } from "../lib/library";
import { useToast } from "../lib/toast";
import { baseName, formatNumber } from "../lib/format";

const APP_VERSION = "0.2.0";

interface FormState {
  masterLibraryRoot: string;
  importConcurrency: number;
  backupWorkers: number;
  maxRetries: number;
  defaultEventName: string;
  generateThumbsAfterImport: boolean;
}

/** Settings — Master Library location, concurrency/worker/retry counts, and defaults. */
export function SettingsPage() {
  const toast = useToast();
  const { current } = useLibrary();
  const [form, setForm] = useState<FormState | null>(null);
  const [metadataAvailable, setMetadataAvailable] = useState(true);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [rootError, setRootError] = useState<string | null>(null);
  const [recent, setRecent] = useState<RecentLibraryDTO[]>([]);
  const [switching, setSwitching] = useState(false);
  const [versionFull, setVersionFull] = useState<string>("");
  // Per-machine "Pause backups while imports run" preference. It is applied live
  // (not part of the Save-changes form) so its own toggle persists immediately.
  const [pauseBackups, setPauseBackups] = useState<boolean | null>(null);
  const [savingPauseBackups, setSavingPauseBackups] = useState(false);

  useEffect(() => {
    let cancelled = false;
    AppService.Version()
      .then((v) => {
        if (!cancelled) setVersionFull(v.full);
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const r = await LibraryService.Recent();
        if (!cancelled) setRecent(r ?? []);
      } catch {
        /* recent list is non-critical */
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const on = await BackupService.PauseBackupsDuringForeground();
        if (!cancelled) setPauseBackups(on);
      } catch {
        /* per-machine backup preference is non-critical */
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const togglePauseBackups = async (on: boolean) => {
    setSavingPauseBackups(true);
    setPauseBackups(on); // optimistic
    try {
      const applied = await BackupService.SetPauseBackupsDuringForeground(on);
      setPauseBackups(applied);
      toast.success(applied ? "Backups will pause during imports" : "Backups no longer pause during imports");
    } catch (e) {
      setPauseBackups(!on); // revert
      toast.fromError(e, "Could not change the setting");
    } finally {
      setSavingPauseBackups(false);
    }
  };

  const switchLibrary = async (path: string) => {
    setSwitching(true);
    try {
      const res = await LibraryService.Open(path);
      if (res.needsRelaunch) {
        toast.success("Library selected — quit and reopen PAIM to switch.");
      } else if (res.lockConflict) {
        toast.fromError(new Error(res.lockConflict.message), "Library is locked");
      }
    } catch (e) {
      toast.fromError(e, "Could not switch library");
    } finally {
      setSwitching(false);
    }
  };

  // Pick a folder that already holds a PAIM library and open it. Open validates
  // <root>/.paim/paim.db server-side; a bad folder surfaces its error as a toast.
  const openAnotherLibrary = async () => {
    setSwitching(true);
    try {
      const path = await LibraryService.PickLibraryFolder();
      if (!path) return; // dialog cancelled
      await switchLibrary(path);
    } catch (e) {
      toast.fromError(e, "Could not open that library");
    } finally {
      setSwitching(false);
    }
  };

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const s = await SettingsService.GetAll();
        if (cancelled) return;
        setForm({
          masterLibraryRoot: s.masterLibraryRoot,
          importConcurrency: s.importConcurrency,
          backupWorkers: s.backupWorkers,
          maxRetries: s.maxRetries,
          defaultEventName: s.defaultEventName,
          generateThumbsAfterImport: s.generateThumbsAfterImport,
        });
        setMetadataAvailable(s.metadataAvailable);
      } catch (e) {
        toast.fromError(e, "Failed to load settings");
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const update = <K extends keyof FormState>(key: K, value: FormState[K]) => {
    setForm((f) => (f ? { ...f, [key]: value } : f));
  };

  const save = async () => {
    if (!form) return;
    setSaving(true);
    setRootError(null);
    try {
      const payload: Settings = {
        masterLibraryRoot: form.masterLibraryRoot.trim(),
        importConcurrency: clampInt(form.importConcurrency, 1),
        backupWorkers: clampInt(form.backupWorkers, 1),
        maxRetries: clampInt(form.maxRetries, 0),
        defaultEventName: form.defaultEventName.trim(),
        generateThumbsAfterImport: form.generateThumbsAfterImport,
        metadataAvailable,
      };
      const saved = await SettingsService.Update(payload);
      setForm({
        masterLibraryRoot: saved.masterLibraryRoot,
        importConcurrency: saved.importConcurrency,
        backupWorkers: saved.backupWorkers,
        maxRetries: saved.maxRetries,
        defaultEventName: saved.defaultEventName,
        generateThumbsAfterImport: saved.generateThumbsAfterImport,
      });
      setMetadataAvailable(saved.metadataAvailable);
      toast.success("Settings saved");
    } catch (e) {
      // The most common rejection is an invalid Master Library path.
      const msg = toast.fromError(e, "Could not save settings");
      setRootError(msg);
    } finally {
      setSaving(false);
    }
  };

  if (loading || !form) {
    return (
      <div>
        <PageHeader title="Settings" />
        <LoadingBlock label="Loading settings…" />
      </div>
    );
  }

  return (
    <div>
      <PageHeader
        title="Settings"
        description="Configure your Master Library location and how PAIM imports and backs up. Worker, retry, and concurrency changes take effect on the next launch."
        actions={
          <Button variant="primary" icon={CheckCircleIcon} onClick={() => void save()} loading={saving}>
            Save changes
          </Button>
        }
      />

      <div className="space-y-4">
        <Card
          title="Library"
          subtitle="The library root is your Master Library — the catalog lives inside it and travels with your photos."
        >
          <dl className="grid gap-3 sm:grid-cols-2">
            <div className="sm:col-span-2 flex items-start justify-between gap-3">
              <dt className="flex items-center gap-1.5 text-[12px] text-zinc-500">
                <CircleStackIcon className="h-4 w-4 flex-none" /> Location
              </dt>
              <dd className="min-w-0 break-all text-right font-mono text-[12px] text-zinc-300">
                {current?.path ?? "—"}
              </dd>
            </div>
            <AboutRow label="Name" value={current?.name ?? "—"} />
            <AboutRow label="Schema version" value={current ? `v${current.schemaVersion}` : "—"} />
            <AboutRow label="Opened by app" value={current?.appVersion || APP_VERSION} />
          </dl>
          {rootError ? (
            <p className="mt-2 flex items-start gap-1.5 text-[12px] text-red-400">
              <ExclamationTriangleIcon className="mt-0.5 h-4 w-4 flex-none" />
              {rootError}
            </p>
          ) : null}

          {recent.length > 1 ? (
            <div className="mt-4 border-t border-zinc-800 pt-3">
              <div className="mb-2 text-[12px] font-medium text-zinc-400">Recent libraries</div>
              <ul className="divide-y divide-zinc-800">
                {recent.map((r) => {
                  const isCurrent = r.path === current?.path;
                  return (
                    <li key={r.path} className="flex items-center justify-between gap-3 py-2">
                      <div className="min-w-0">
                        <div className="truncate text-[12px] text-zinc-300">{r.name}</div>
                        <div className="truncate font-mono text-[11px] text-zinc-500">{r.path}</div>
                      </div>
                      <Button
                        size="sm"
                        variant="ghost"
                        icon={ArrowsRightLeftIcon}
                        disabled={isCurrent || switching}
                        onClick={() => void switchLibrary(r.path)}
                      >
                        {isCurrent ? "Current" : "Switch"}
                      </Button>
                    </li>
                  );
                })}
              </ul>
              <p className="mt-2 text-[11px] text-zinc-500">
                Switching updates your choice; quit and reopen PAIM to load the other library.
              </p>
            </div>
          ) : null}

          <div className="mt-4 border-t border-zinc-800 pt-3">
            <div className="flex items-start justify-between gap-3">
              <div className="min-w-0">
                <div className="text-[12px] font-medium text-zinc-400">Open another library</div>
                <p className="mt-1 max-w-lg text-[11px] text-zinc-500">
                  A library is a folder containing your photos and their catalog — open copies of a catalog only as
                  read-only insurance; changes made in a copy never merge back.
                </p>
              </div>
              <Button
                size="sm"
                variant="secondary"
                icon={FolderOpenIcon}
                onClick={() => void openAnotherLibrary()}
                loading={switching}
              >
                Open another library…
              </Button>
            </div>
          </div>

          <ReorganizeSection />
        </Card>

        <Card title="Import">
          <div className="grid gap-4 sm:grid-cols-2">
            <NumberField
              label="Import concurrency"
              hint="Files hashed/copied in parallel during import."
              value={form.importConcurrency}
              min={1}
              onChange={(v) => update("importConcurrency", v)}
            />
            <TextField
              label="Default event name"
              hint="Pre-fills the event folder name for new imports."
              value={form.defaultEventName}
              placeholder="e.g. Untitled"
              onChange={(v) => update("defaultEventName", v)}
            />
          </div>
          <label className="mt-4 flex items-start gap-2.5">
            <input
              type="checkbox"
              checked={form.generateThumbsAfterImport}
              onChange={(e) => update("generateThumbsAfterImport", e.target.checked)}
              className="mt-0.5 h-4 w-4 flex-none rounded border-zinc-600 bg-zinc-950 text-blue-500 focus:ring-blue-500"
            />
            <span>
              <span className="text-[13px] text-zinc-200">Generate thumbnails after import</span>
              <span className="mt-0.5 block text-[11px] text-zinc-500">
                When an import or adopt finishes, pre-generate its thumbnails in the background so browsing is instant.
                Runs after the import completes — never during it.
              </span>
            </span>
          </label>
        </Card>

        <ThumbnailsCard />

        <SnapshotsCard />

        <Card title="Backup">
          <div className="grid gap-4 sm:grid-cols-2">
            <NumberField
              label="Backup workers"
              hint="Concurrent upload workers in the backup queue."
              value={form.backupWorkers}
              min={1}
              onChange={(v) => update("backupWorkers", v)}
            />
            <NumberField
              label="Max retries"
              hint="Attempts per job before it is marked failed (exponential backoff)."
              value={form.maxRetries}
              min={0}
              onChange={(v) => update("maxRetries", v)}
            />
          </div>
          <p className="mt-3 flex items-start gap-1.5 text-[11px] text-zinc-500">
            <InformationCircleIcon className="mt-0.5 h-3.5 w-3.5 flex-none" />
            Worker and retry counts are read at startup — restart PAIM for changes to take effect.
          </p>

          <label className="mt-4 flex items-start gap-2.5 border-t border-zinc-800 pt-4">
            <input
              type="checkbox"
              checked={pauseBackups ?? true}
              disabled={pauseBackups == null || savingPauseBackups}
              onChange={(e) => void togglePauseBackups(e.target.checked)}
              className="mt-0.5 h-4 w-4 flex-none rounded border-zinc-600 bg-zinc-950 text-blue-500 focus:ring-blue-500 disabled:opacity-60"
            />
            <span>
              <span className="text-[13px] text-zinc-200">Pause backups while imports run</span>
              <span className="mt-0.5 block text-[11px] text-zinc-500">
                Recommended for libraries on hard drives — backups resume automatically when the import finishes.
              </span>
            </span>
          </label>
        </Card>

        <Card title="About">
          <dl className="grid gap-3 sm:grid-cols-2">
            <AboutRow label="Application" value="Photo Archive Integrity Manager (PAIM)" />
            <AboutRow label="Version" value={versionFull || APP_VERSION} />
            <div className="flex items-center justify-between gap-2">
              <dt className="text-[12px] text-zinc-500">Metadata (exiftool)</dt>
              <dd>
                <StatusBadge
                  status={metadataAvailable ? "available" : "missing"}
                  tone={metadataAvailable ? "success" : "warn"}
                  label={metadataAvailable ? "Available" : "Not detected"}
                  dot
                />
              </dd>
            </div>
          </dl>
          {!metadataAvailable ? (
            <p className="mt-3 flex items-start gap-1.5 text-[11px] text-amber-300/80">
              <ExclamationTriangleIcon className="mt-0.5 h-3.5 w-3.5 flex-none" />
              exiftool was not found. Imports proceed with reduced metadata (capture date falls back to file modification
              time).
            </p>
          ) : null}
        </Card>
      </div>
    </div>
  );
}

/* --------------------------- Reorganize library --------------------------- */

const REORG_MOVE_PREVIEW = 8;

/**
 * ReorganizeSection — a maintenance control inside the Library card. It previews
 * a catalog-driven plan (PlanReorganize), gates the run behind a typed
 * confirmation, then runs it (StartReorganize) rendering live progress from the
 * shared import:progress / import:completed events (phase "reorganizing").
 */
function ReorganizeSection() {
  const toast = useToast();
  const [plan, setPlan] = useState<ReorganizePlanDTO | null>(null);
  const [planning, setPlanning] = useState(false);
  const [planProgress, setPlanProgress] = useState<ReorganizePlanProgress | null>(null);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [starting, setStarting] = useState(false);
  const [running, setRunning] = useState(false);
  const [sessionId, setSessionId] = useState<string | null>(null);
  const [progress, setProgress] = useState<ImportProgress | null>(null);
  const [completed, setCompleted] = useState<ImportCompleted | null>(null);
  const [cancelling, setCancelling] = useState(false);
  // "Use original folder names as labels" — default ON (the UI default; the
  // engine option itself defaults off). When on, an empty event derives each
  // file's label from its current parent folder during the reorganize.
  const [useSourceFolderLabels, setUseSourceFolderLabels] = useState(true);
  // Another import (not this reorganize) is active → the one-active guard would
  // reject a start, so disable the entry point and explain why.
  const [busyElsewhere, setBusyElsewhere] = useState(false);

  // Detect an already-running operation on mount.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const active = await ImportService.ActiveImport();
        if (!cancelled && active && !active.done) {
          if (active.phase === "reorganizing") {
            setRunning(true);
            setSessionId(active.sessionId);
            setProgress(active);
          } else {
            setBusyElsewhere(true);
          }
        }
      } catch {
        /* non-critical */
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  useWailsEvent<ImportProgress>(WailsEvents.ImportProgress, (data) => {
    if (running && (sessionId === null || data.sessionId === sessionId)) {
      setProgress(data);
    } else if (!running && data.phase !== "reorganizing" && !data.done) {
      setBusyElsewhere(true);
    }
  });

  useWailsEvent<ReorganizePlanProgress>(WailsEvents.ReorganizePlanProgress, (data) => {
    setPlanProgress(data);
  });

  useWailsEvent<ImportCompleted>(WailsEvents.ImportCompleted, (data) => {
    if (running && (sessionId === null || data.sessionId === sessionId)) {
      setRunning(false);
      setCompleted(data);
      if (data.status === "completed") {
        toast.success("Reorganize complete", `${formatNumber(data.filesImported)} file(s) moved into place.`);
      } else if (data.status === "cancelled") {
        toast.info("Reorganize cancelled", "Files already moved are kept; the rest were left in place.");
      } else {
        toast.warn("Reorganize finished with issues", `${formatNumber(data.failures)} failure(s) — see the Logs page.`);
      }
    } else {
      setBusyElsewhere(false);
    }
  });

  const preview = async () => {
    setPlanning(true);
    setPlanProgress(null);
    setPlan(null);
    setCompleted(null);
    try {
      const p = await ImportService.PlanReorganize("", useSourceFolderLabels);
      setPlan(p);
    } catch (e) {
      toast.fromError(e, "Could not compute the reorganize plan");
    } finally {
      setPlanning(false);
      setPlanProgress(null);
    }
  };

  const start = async () => {
    setStarting(true);
    try {
      const res = await ImportService.StartReorganize();
      setSessionId(res.sessionId);
      setRunning(true);
      setConfirmOpen(false);
      setPlan(null);
      setProgress(null);
      setCompleted(null);
    } catch (e) {
      toast.fromError(e, "Could not start reorganize");
    } finally {
      setStarting(false);
    }
  };

  const cancel = async () => {
    setCancelling(true);
    try {
      await ImportService.CancelImport();
      toast.info("Cancelling reorganize…");
    } catch (e) {
      toast.fromError(e, "Could not cancel reorganize");
    } finally {
      setCancelling(false);
    }
  };

  const pct = progress?.percent ?? null;

  return (
    <div className="mt-4 border-t border-zinc-800 pt-4">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-center gap-1.5 text-[13px] font-medium text-zinc-200">
            <FolderArrowDownIcon className="h-4 w-4 flex-none text-zinc-400" />
            Reorganize library
          </div>
          <p className="mt-0.5 text-[11px] text-zinc-500">
            Move already-registered files into the standard{" "}
            <span className="font-mono">YYYY/YYYY-MM-DD/</span> layout. Same-drive moves only — nothing is copied.
          </p>
        </div>
        {!running ? (
          <Button
            size="sm"
            variant="secondary"
            icon={ArrowsRightLeftIcon}
            onClick={() => void preview()}
            loading={planning}
            disabled={busyElsewhere}
          >
            Reorganize library…
          </Button>
        ) : null}
      </div>

      {busyElsewhere && !running ? (
        <p className="mt-2 flex items-start gap-1.5 text-[11px] text-amber-300/80">
          <ExclamationTriangleIcon className="mt-0.5 h-3.5 w-3.5 flex-none" />
          An import is currently running. Wait for it to finish before reorganizing.
        </p>
      ) : null}

      {!running ? (
        <label className="mt-2.5 flex cursor-pointer items-start gap-2 text-[12px] text-zinc-300">
          <input
            type="checkbox"
            checked={useSourceFolderLabels}
            onChange={(e) => {
              setUseSourceFolderLabels(e.target.checked);
              setPlan(null); // a changed option invalidates any previewed plan
            }}
            className="mt-0.5 h-3.5 w-3.5 flex-none rounded border-zinc-600 bg-zinc-950 text-blue-500 focus:ring-blue-500"
          />
          <span>
            Use original folder names as labels
            <span className="mt-0.5 block text-[11px] text-zinc-500">
              Each file&rsquo;s existing parent folder becomes the event label in{" "}
              <span className="font-mono">YYYY-MM-DD label/</span>. Generic camera folders (DCIM, 100CANON…) and
              plain dates are ignored.
            </span>
          </span>
        </label>
      ) : null}

      {/* Determinate progress while the plan is being computed from the catalog. */}
      {planning ? (
        <div className="mt-3 rounded-md border border-zinc-800 bg-zinc-950/40 p-3">
          <div className="mb-1 flex items-center justify-between text-[11px] text-zinc-400">
            <span>Computing plan…</span>
            {planProgress && planProgress.total > 0 ? (
              <span className="tabular-nums">
                {formatNumber(planProgress.done)} / {formatNumber(planProgress.total)}
              </span>
            ) : null}
          </div>
          <div className="h-1.5 w-full overflow-hidden rounded-full bg-zinc-800">
            <div
              className="h-full rounded-full bg-blue-500 transition-all"
              style={{
                width:
                  planProgress && planProgress.total > 0
                    ? `${Math.min(100, Math.round((planProgress.done / planProgress.total) * 100))}%`
                    : "15%",
              }}
            />
          </div>
        </div>
      ) : null}

      {/* Plan preview (idle, plan computed, not yet running). */}
      {plan && !running ? (
        <div className="mt-3 rounded-md border border-zinc-800 bg-zinc-950/40 p-3">
          <div className="grid grid-cols-3 gap-2 text-center">
            <PlanStat label="To move" value={plan.moves} tone="accent" />
            <PlanStat label="Already in place" value={plan.inPlace} />
            <PlanStat label="Skipped" value={plan.skipped} tone={plan.skipped > 0 ? "warn" : "default"} />
          </div>

          {plan.moves > 0 ? (
            <div className="mt-3">
              <div className="mb-1 text-[11px] font-medium text-zinc-400">Planned moves</div>
              <ul className="space-y-1">
                {(plan.movesSample ?? []).slice(0, REORG_MOVE_PREVIEW).map((m) => (
                  <li key={m.assetId} className="flex items-center gap-1.5 truncate font-mono text-[11px] text-zinc-400">
                    <span className="truncate text-zinc-500" title={m.from}>
                      {baseName(m.from)}
                    </span>
                    <ArrowsRightLeftIcon className="h-3 w-3 flex-none text-zinc-600" />
                    <span className="truncate text-zinc-300" title={m.to}>
                      {relTo(m.to)}
                    </span>
                  </li>
                ))}
              </ul>
              <p className="mt-1.5 text-[10px] text-zinc-600">
                Showing {Math.min(plan.movesSample?.length ?? 0, REORG_MOVE_PREVIEW)} of {formatNumber(plan.moves)} move
                {plan.moves === 1 ? "" : "s"}
                {plan.truncated ? " (list capped)" : ""}.
              </p>
            </div>
          ) : null}

          {plan.skipped > 0 ? (
            <p className="mt-2 text-[11px] text-zinc-500">
              {formatNumber(plan.skipped)} skipped (missing on disk, cross-volume, or flagged duplicate) — left untouched.
            </p>
          ) : null}

          <div className="mt-3 flex items-center justify-end gap-2">
            <Button size="sm" variant="ghost" onClick={() => setPlan(null)}>
              Dismiss
            </Button>
            <Button
              size="sm"
              variant="primary"
              icon={FolderArrowDownIcon}
              onClick={() => setConfirmOpen(true)}
              disabled={plan.moves === 0 || busyElsewhere}
            >
              Reorganize {formatNumber(plan.moves)} file{plan.moves === 1 ? "" : "s"}
            </Button>
          </div>
        </div>
      ) : null}

      {/* Live progress while running. */}
      {running ? (
        <div className="mt-3 rounded-md border border-blue-500/30 bg-blue-500/5 p-3">
          <div className="mb-2 flex items-center justify-between">
            <div className="flex items-center gap-2 text-[12px] font-medium text-zinc-200">
              <ArrowPathIcon className="h-4 w-4 animate-spin text-blue-400" />
              Reorganizing…
            </div>
            <Button size="sm" variant="danger" icon={StopIcon} onClick={() => void cancel()} loading={cancelling}>
              Cancel
            </Button>
          </div>
          <ProgressBar
            percent={pct}
            striped
            size="md"
            label={progress?.currentFile ? baseName(progress.currentFile) : "Preparing…"}
            detail={
              progress ? `${formatNumber(progress.filesDone)} / ${formatNumber(progress.filesTotal)} files` : undefined
            }
          />
        </div>
      ) : null}

      {/* Completion summary. */}
      {completed && !running ? (
        <div className="mt-3 flex items-center justify-between gap-3 rounded-md border border-zinc-800 bg-zinc-950/40 p-3">
          <div className="flex items-center gap-2 text-[12px] text-zinc-300">
            <StatusBadge status={completed.status} />
            <span>
              {formatNumber(completed.filesImported)} moved · {formatNumber(completed.skipped)} skipped ·{" "}
              {formatNumber(completed.failures)} failed
            </span>
          </div>
          <Button size="sm" variant="ghost" onClick={() => setCompleted(null)}>
            Done
          </Button>
        </div>
      ) : null}

      <ConfirmDialog
        open={confirmOpen}
        title="Reorganize your library?"
        variant="primary"
        requireWord="REORGANIZE"
        confirmLabel={`Reorganize ${plan ? formatNumber(plan.moves) : ""} file${plan?.moves === 1 ? "" : "s"}`}
        cancelLabel="Keep as-is"
        loading={starting}
        description={
          <div className="space-y-2">
            <p>
              This moves {plan ? formatNumber(plan.moves) : ""} already-archived file(s) into the standard layout. Moves
              are <span className="font-medium text-zinc-300">same-drive renames</span> — no copying, and every file is
              re-verified at its new location.
            </p>
            <p>
              Your files&rsquo; folder structure will change, and this cannot be automatically undone. Files that are
              missing, on another volume, or flagged duplicates are left untouched.
            </p>
          </div>
        }
        onConfirm={() => void start()}
        onCancel={() => setConfirmOpen(false)}
      />
    </div>
  );
}

/* ------------------------------- Thumbnails ------------------------------- */

/**
 * ThumbnailsCard — per-machine thumbnail cache location (in-library vs this Mac's
 * local disk), a "Clear cache" action, and a "Pre-generate all thumbnails"
 * warm-up with live progress. Thumbnails are a disposable cache.
 */
function ThumbnailsCard() {
  const toast = useToast();
  const [cache, setCache] = useState<ThumbCacheDTO | null>(null);
  const [saving, setSaving] = useState(false);
  const [clearing, setClearing] = useState(false);
  const [warm, setWarm] = useState<WarmupStatusDTO | null>(null);
  const [starting, setStarting] = useState(false);
  const [parallelism, setParallelismState] = useState<number | null>(null);
  const [savingParallelism, setSavingParallelism] = useState(false);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const [c, w, p] = await Promise.all([
          ThumbnailService.ThumbnailCacheLocation(),
          ThumbnailService.WarmupStatus(),
          ThumbnailService.ThumbnailParallelism(),
        ]);
        if (cancelled) return;
        setCache(c);
        setWarm(w);
        setParallelismState(p);
      } catch {
        /* non-critical */
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const saveParallelism = async (n: number) => {
    setSavingParallelism(true);
    try {
      const applied = await ThumbnailService.SetThumbnailParallelism(n);
      setParallelismState(applied);
      toast.success("Thumbnail parallelism updated", "Applies immediately to browsing and warm-up.");
    } catch (e) {
      toast.fromError(e, "Could not change parallelism");
    } finally {
      setSavingParallelism(false);
    }
  };

  useWailsEvent<ThumbsProgress>(WailsEvents.ThumbsProgress, (p) => {
    setWarm({ running: p.running, done: p.done, total: p.total, label: p.label });
  });

  const setLocation = async (location: string) => {
    if (!cache || location === cache.location) return;
    setSaving(true);
    try {
      const updated = await ThumbnailService.SetThumbnailCacheLocation(location);
      setCache(updated);
      toast.success("Thumbnail cache location updated", "New thumbnails use the new location; the old cache is left as-is.");
    } catch (e) {
      toast.fromError(e, "Could not change cache location");
    } finally {
      setSaving(false);
    }
  };

  const clear = async () => {
    setClearing(true);
    try {
      await ThumbnailService.ClearThumbnailCache();
      toast.success("Thumbnail cache cleared", "Thumbnails regenerate on demand as you browse.");
    } catch (e) {
      toast.fromError(e, "Could not clear cache");
    } finally {
      setClearing(false);
    }
  };

  const warmAll = async () => {
    setStarting(true);
    try {
      const st = await ThumbnailService.StartWarmupAll();
      setWarm(st);
    } catch (e) {
      toast.fromError(e, "Could not start warm-up");
    } finally {
      setStarting(false);
    }
  };

  const cancelWarm = async () => {
    try {
      await ThumbnailService.CancelWarmup();
    } catch (e) {
      toast.fromError(e, "Could not cancel warm-up");
    }
  };

  const running = !!warm?.running;
  const pct = warm && warm.total > 0 ? Math.min(100, Math.round((warm.done / warm.total) * 100)) : null;

  return (
    <Card
      title="Thumbnails"
      subtitle="Thumbnails are a disposable cache — clearing or moving them never touches your photos."
    >
      <div className="space-y-3">
        <RadioRow
          checked={cache?.location === "library"}
          disabled={saving || !cache}
          onSelect={() => void setLocation("library")}
          title="In the library (default)"
          hint={cache?.libraryDir}
        />
        <RadioRow
          checked={cache?.location === "local"}
          disabled={saving || !cache}
          onSelect={() => void setLocation("local")}
          title="On this Mac's internal disk"
          hint={cache?.localDir}
        />
        <p className="flex items-start gap-1.5 text-[11px] text-zinc-500">
          <InformationCircleIcon className="mt-0.5 h-3.5 w-3.5 flex-none" />
          Keeping thumbnails on this Mac's internal disk makes browsing faster for libraries on slow drives. Switching
          takes effect immediately; the old cache is left where it is (delete it with Clear if you like).
        </p>
      </div>

      <div className="mt-4 border-t border-zinc-800 pt-4">
        <label className="flex flex-wrap items-center justify-between gap-3">
          <span className="min-w-0">
            <span className="block text-[13px] text-zinc-200">Thumbnail generation parallelism</span>
            <span className="mt-0.5 block max-w-lg text-[11px] text-zinc-500">
              How many thumbnails PAIM renders at once — shared by browsing and warm-up. Keep it low (1&ndash;2) for a
              spinning external HDD, where parallel renders cause seek thrash; raise it for a fast SSD. Applies
              immediately.
            </span>
          </span>
          <input
            type="number"
            min={1}
            max={16}
            disabled={parallelism == null || savingParallelism}
            value={parallelism ?? 2}
            onChange={(e) => setParallelismState(e.target.value === "" ? 1 : Number(e.target.value))}
            onBlur={(e) => {
              const n = clampInt(Number(e.target.value), 1);
              if (n !== parallelism) void saveParallelism(n);
            }}
            className="w-20 flex-none rounded-md border border-zinc-700 bg-zinc-950 px-3 py-1.5 text-[13px] text-zinc-200 outline-none focus:border-blue-500 disabled:opacity-60"
          />
        </label>
      </div>

      <div className="mt-4 flex flex-wrap items-center gap-2 border-t border-zinc-800 pt-4">
        {!running ? (
          <Button size="sm" variant="secondary" icon={BoltIcon} onClick={() => void warmAll()} loading={starting}>
            Pre-generate all thumbnails
          </Button>
        ) : (
          <Button size="sm" variant="danger" icon={StopIcon} onClick={() => void cancelWarm()}>
            Cancel warm-up
          </Button>
        )}
        <Button size="sm" variant="ghost" icon={TrashIcon} onClick={() => void clear()} loading={clearing}>
          Clear thumbnail cache
        </Button>
      </div>

      {running ? (
        <div className="mt-3 rounded-md border border-blue-500/30 bg-blue-500/5 p-3">
          <div className="mb-1 flex items-center justify-between text-[11px] text-zinc-400">
            <span className="flex items-center gap-1.5">
              <PhotoIcon className="h-3.5 w-3.5" /> Generating thumbnails
            </span>
            <span className="tabular-nums">
              {formatNumber(warm?.done ?? 0)} of {formatNumber(warm?.total ?? 0)}
            </span>
          </div>
          <ProgressBar percent={pct} striped size="sm" />
        </div>
      ) : null}
    </Card>
  );
}

function RadioRow({
  checked,
  disabled,
  onSelect,
  title,
  hint,
}: {
  checked?: boolean;
  disabled?: boolean;
  onSelect: () => void;
  title: string;
  hint?: string;
}) {
  return (
    <button
      type="button"
      disabled={disabled}
      onClick={onSelect}
      className="flex w-full items-start gap-2.5 rounded-md border border-zinc-800 bg-zinc-950/40 p-2.5 text-left transition hover:border-zinc-700 disabled:opacity-60"
    >
      <span
        className={`mt-0.5 flex h-4 w-4 flex-none items-center justify-center rounded-full border ${
          checked ? "border-blue-500" : "border-zinc-600"
        }`}
      >
        {checked ? <span className="h-2 w-2 rounded-full bg-blue-500" /> : null}
      </span>
      <span className="min-w-0">
        <span className="block text-[13px] text-zinc-200">{title}</span>
        {hint ? <span className="block truncate font-mono text-[11px] text-zinc-500">{hint}</span> : null}
      </span>
    </button>
  );
}

/* ---------------------------- Catalog snapshots --------------------------- */

const SNAPSHOT_INTERVALS = [
  { value: "off", label: "Off" },
  { value: "quit", label: "On quit only" },
  { value: "6h", label: "Every 6 hours" },
  { value: "daily", label: "Daily" },
];

/**
 * SnapshotsCard — configure a one-way, disaster-recovery destination + interval
 * for catalog snapshots, run one on demand, and see the last-run status. A
 * snapshot is insurance only; it is never opened as the live catalog.
 */
function SnapshotsCard() {
  const toast = useToast();
  const [status, setStatus] = useState<SnapshotStatusDTO | null>(null);
  const [dest, setDest] = useState("");
  const [interval, setIntervalVal] = useState("daily");
  const [saving, setSaving] = useState(false);
  const [running, setRunning] = useState(false);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const s = await SnapshotService.SnapshotStatus();
        if (cancelled) return;
        setStatus(s);
        setDest(s.dest);
        setIntervalVal(s.interval || "daily");
      } catch {
        /* non-critical */
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const pickDest = async () => {
    try {
      const p = await SnapshotService.PickSnapshotDest();
      if (p) setDest(p);
    } catch (e) {
      toast.fromError(e, "Could not open folder picker");
    }
  };

  const saveConfig = async (nextDest: string, nextInterval: string) => {
    setSaving(true);
    try {
      const s = await SnapshotService.SetSnapshotConfig({ dest: nextDest, interval: nextInterval });
      setStatus(s);
      setDest(s.dest);
      setIntervalVal(s.interval);
      toast.success("Snapshot settings saved");
    } catch (e) {
      toast.fromError(e, "Could not save snapshot settings");
    } finally {
      setSaving(false);
    }
  };

  const snapshotNow = async () => {
    setRunning(true);
    try {
      const s = await SnapshotService.SnapshotNow();
      setStatus(s);
      if (s.lastError) {
        toast.warn("Snapshot failed", s.lastError);
      } else {
        toast.success("Snapshot written", s.lastPath);
      }
    } catch (e) {
      toast.fromError(e, "Could not snapshot now");
    } finally {
      setRunning(false);
    }
  };

  return (
    <Card
      title="Catalog snapshots"
      subtitle="One-way, disaster-recovery copies of your catalog database. Insurance only — a snapshot is never opened as the live library."
    >
      <div className="space-y-3">
        <div>
          <span className="mb-1 block text-[11px] font-medium text-zinc-500">Destination folder</span>
          <div className="flex items-center gap-2">
            <div className="min-w-0 flex-1 truncate rounded-md border border-zinc-700 bg-zinc-950 px-3 py-1.5 font-mono text-[12px] text-zinc-300">
              {dest || <span className="text-zinc-600">No destination — snapshots are off</span>}
            </div>
            <Button size="sm" variant="secondary" icon={FolderOpenIcon} onClick={() => void pickDest()}>
              Choose…
            </Button>
          </div>
        </div>

        <label className="block">
          <span className="mb-1 block text-[11px] font-medium text-zinc-500">Interval</span>
          <select
            value={interval}
            onChange={(e) => setIntervalVal(e.target.value)}
            className="rounded-md border border-zinc-700 bg-zinc-950 px-2.5 py-1.5 text-[13px] text-zinc-200 outline-none focus:border-blue-500"
          >
            {SNAPSHOT_INTERVALS.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        </label>

        <div className="flex flex-wrap items-center gap-2">
          <Button size="sm" variant="primary" icon={CheckCircleIcon} onClick={() => void saveConfig(dest, interval)} loading={saving}>
            Save snapshot settings
          </Button>
          <Button
            size="sm"
            variant="secondary"
            icon={CameraIcon}
            onClick={() => void snapshotNow()}
            loading={running}
            disabled={!dest}
          >
            Snapshot now
          </Button>
        </div>

        {status ? (
          <div className="rounded-md border border-zinc-800 bg-zinc-950/40 p-3 text-[12px]">
            {status.lastError ? (
              <p className="flex items-start gap-1.5 text-red-400">
                <ExclamationTriangleIcon className="mt-0.5 h-4 w-4 flex-none" />
                Last snapshot failed: {status.lastError}
              </p>
            ) : status.lastAt ? (
              <div className="space-y-0.5">
                <div className="text-zinc-300">Last snapshot {new Date(status.lastAt).toLocaleString()}</div>
                {status.lastPath ? (
                  <div className="truncate font-mono text-[11px] text-zinc-500" title={status.lastPath}>
                    {status.lastPath}
                  </div>
                ) : null}
              </div>
            ) : (
              <p className="text-zinc-500">No snapshot taken yet.</p>
            )}
          </div>
        ) : null}
      </div>
    </Card>
  );
}

/** relTo trims a long absolute destination to its last two path segments. */
function relTo(p: string): string {
  const parts = p.split("/").filter(Boolean);
  if (parts.length <= 2) return p;
  return "…/" + parts.slice(-2).join("/");
}

function PlanStat({
  label,
  value,
  tone = "default",
}: {
  label: string;
  value: number;
  tone?: "default" | "accent" | "warn";
}) {
  const color = tone === "accent" ? "text-blue-400" : tone === "warn" ? "text-amber-400" : "text-zinc-100";
  return (
    <div className="rounded-md border border-zinc-800 bg-zinc-900/60 p-2">
      <div className={`text-lg font-semibold tabular-nums ${color}`}>{formatNumber(value)}</div>
      <div className="mt-0.5 text-[10px] text-zinc-500">{label}</div>
    </div>
  );
}

function clampInt(v: number, min: number): number {
  const n = Math.floor(Number(v));
  if (!isFinite(n) || n < min) return min;
  return n;
}

function NumberField({
  label,
  hint,
  value,
  min,
  onChange,
}: {
  label: string;
  hint?: string;
  value: number;
  min: number;
  onChange: (v: number) => void;
}) {
  return (
    <label className="block">
      <span className="text-xs font-medium text-zinc-400">{label}</span>
      <input
        type="number"
        min={min}
        value={value}
        onChange={(e) => onChange(e.target.value === "" ? min : Number(e.target.value))}
        className="mt-1.5 w-full rounded-md border border-zinc-700 bg-zinc-950 px-3 py-1.5 text-[13px] text-zinc-200 outline-none focus:border-blue-500"
      />
      {hint ? <span className="mt-1 block text-[11px] text-zinc-500">{hint}</span> : null}
    </label>
  );
}

function TextField({
  label,
  hint,
  value,
  placeholder,
  onChange,
}: {
  label: string;
  hint?: string;
  value: string;
  placeholder?: string;
  onChange: (v: string) => void;
}) {
  return (
    <label className="block">
      <span className="text-xs font-medium text-zinc-400">{label}</span>
      <input
        value={value}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
        className="mt-1.5 w-full rounded-md border border-zinc-700 bg-zinc-950 px-3 py-1.5 text-[13px] text-zinc-200 outline-none focus:border-blue-500"
      />
      {hint ? <span className="mt-1 block text-[11px] text-zinc-500">{hint}</span> : null}
    </label>
  );
}

function AboutRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center justify-between gap-2">
      <dt className="text-[12px] text-zinc-500">{label}</dt>
      <dd className="text-[12px] text-zinc-300">{value}</dd>
    </div>
  );
}
