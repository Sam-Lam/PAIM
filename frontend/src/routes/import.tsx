import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import {
  ArrowLeftIcon,
  ArrowPathIcon,
  CheckCircleIcon,
  DocumentDuplicateIcon,
  ExclamationTriangleIcon,
  FilmIcon,
  FolderOpenIcon,
  InformationCircleIcon,
  PhotoIcon,
  PlayIcon,
  ShieldCheckIcon,
  Squares2X2Icon,
  StopIcon,
} from "@heroicons/react/24/outline";
import { Button, Card, ClearSourceControl, ConfirmDialog, FailedFilesPanel, LoadingBlock, PageHeader, ProgressBar, StatusBadge } from "../components";
import {
  BackupService,
  HistoryService,
  ImportOptions,
  ImportService,
  ProviderService,
  SourcesService,
  SettingsService,
  WailsEvents,
  type AnalyzeCompleted,
  type DryRunReportDTO,
  type ImportCompleted,
  type ImportProgress,
  type ProviderDTO,
  type SafeToEraseDTO,
  type SessionBackupStatusDTO,
  type SessionDTO,
  type Settings,
  type SourceEvaluated,
  type SourceProgress,
} from "../lib/api";
import { useWailsEvent } from "../lib/hooks";
import { useToast } from "../lib/toast";
import { baseName, formatBytes, formatEta, formatNumber, formatRate } from "../lib/format";

type Mode = "copy" | "adopt";
type Step = 1 | 2 | 3;

export function ImportPage() {
  const toast = useToast();
  const navigate = useNavigate();
  const search = useSearch({ strict: false }) as { root?: string };

  const [step, setStep] = useState<Step>(1);

  // Step 1 — source configuration.
  const [root, setRoot] = useState("");
  const [mode, setMode] = useState<Mode>("copy");
  const [reorganize, setReorganize] = useState(false);
  const [eventName, setEventName] = useState("");
  const [settings, setSettings] = useState<Settings | null>(null);
  // Enabled backup destinations and the per-import opt-out set (provider IDs the
  // user unchecked in the "Back up to" section). Default: back up to all.
  const [enabledProviders, setEnabledProviders] = useState<ProviderDTO[]>([]);
  const [skipProviderIds, setSkipProviderIds] = useState<string[]>([]);

  // Step 2 — analysis (runs server-side as a background job; re-attachable).
  const [analyzeRunning, setAnalyzeRunning] = useState(false);
  const [analyzeProgress, setAnalyzeProgress] = useState<ImportProgress | null>(null);
  const [dryRun, setDryRun] = useState<DryRunReportDTO | null>(null);

  // Step 3 — running import.
  const [starting, setStarting] = useState(false);
  const [progress, setProgress] = useState<ImportProgress | null>(null);
  const [completed, setCompleted] = useState<ImportCompleted | null>(null);
  const [showCancel, setShowCancel] = useState(false);
  const [cancelling, setCancelling] = useState(false);

  const [interrupted, setInterrupted] = useState<SessionDTO[]>([]);
  const [booting, setBooting] = useState(true);

  const masterRoot = settings?.masterLibraryRoot ?? "";

  // Live progress + completion events.
  //
  // import:progress is shared by imports, reorganizes, and background analyzes.
  // Analyze progress is marked by an empty sessionId; a reorganize carries a
  // sessionId AND phase "reorganizing". We route by sessionId so a background
  // reorganize can never hijack this page's analyze UI, and additionally ignore
  // the reorganizing phase defensively.
  useWailsEvent<ImportProgress>(WailsEvents.ImportProgress, (data) => {
    if (data.sessionId === "") {
      if (data.phase === "reorganizing") return;
      setAnalyzeProgress(data);
    } else {
      setProgress(data);
    }
  });
  useWailsEvent<ImportCompleted>(WailsEvents.ImportCompleted, (data) => {
    setCompleted(data);
    void refreshInterrupted();
  });
  useWailsEvent<AnalyzeCompleted>(WailsEvents.AnalyzeCompleted, (data) => {
    setAnalyzeRunning(false);
    if (data.report) {
      restoreOpts(data.opts);
      setDryRun(data.report);
      setStep(2);
      return;
    }
    // Cancelled or failed: no report — fall back to source, keeping selection.
    if (data.error) toast.fromError(data.error, "Analysis failed");
    setAnalyzeProgress(null);
    setStep(1);
  });

  const refreshInterrupted = useCallback(async () => {
    try {
      const page = await HistoryService.ListSessions(1, 50);
      setInterrupted((page.items ?? []).filter((s) => s.status === "interrupted"));
    } catch {
      // non-fatal
    }
  }, []);

  // Restore the whole step-2 context from an analyze opts echo so navigating
  // away and back never loses the selected root/mode/reorganize/event.
  const restoreOpts = useCallback((o: ImportOptions) => {
    if (o.root) setRoot(o.root);
    if (o.mode === "copy" || o.mode === "adopt") setMode(o.mode);
    setReorganize(!!o.reorganize);
    if (typeof o.eventName === "string") setEventName(o.eventName);
    if (Array.isArray(o.skipProviderIds)) setSkipProviderIds(o.skipProviderIds);
  }, []);

  // On mount: load settings, re-attach to a running/completed import OR analyze,
  // seed the picked root from ?root=, and surface interrupted sessions.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const [s, activeImport, activeAnalyze, providers] = await Promise.all([
          SettingsService.GetAll(),
          ImportService.ActiveImport(),
          ImportService.ActiveAnalyze(),
          ProviderService.List().catch(() => [] as ProviderDTO[]),
        ]);
        if (cancelled) return;
        setSettings(s);
        setEnabledProviders((providers ?? []).filter((p) => p.enabled));
        if (s.defaultEventName) setEventName(s.defaultEventName);
        if (activeImport) {
          // App restarted / resumed elsewhere mid-import: attach at step 3.
          setProgress(activeImport);
          setStep(3);
        } else if (activeAnalyze.state === "running") {
          // Re-attach to the in-flight analyze; events keep it updating.
          restoreOpts(activeAnalyze.opts);
          setAnalyzeProgress(activeAnalyze.progress ?? null);
          setAnalyzeRunning(true);
          setStep(2);
        } else if (activeAnalyze.state === "completed" && activeAnalyze.report) {
          // Analyze finished while away: land straight on the report.
          restoreOpts(activeAnalyze.opts);
          setDryRun(activeAnalyze.report);
          setStep(2);
        }
      } catch (e) {
        toast.fromError(e, "Failed to initialize import");
      } finally {
        if (!cancelled) setBooting(false);
      }
      await refreshInterrupted();
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Apply ?root= once settings are known (only when idle on step 1).
  useEffect(() => {
    if (search.root && step === 1 && !root) setRoot(search.root);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [search.root]);

  const buildOptions = useCallback(
    () =>
      ({
        root,
        destinationRoot: mode === "copy" ? masterRoot : root,
        eventName: eventName.trim(),
        mode,
        reorganize: mode === "adopt" && reorganize,
        sourceId: "",
        // Only send IDs that are still enabled destinations (a provider disabled
        // since selection is no longer relevant).
        skipProviderIds: skipProviderIds.filter((id) => enabledProviders.some((p) => p.id === id)),
      }) satisfies ImportOptions,
    [root, mode, masterRoot, eventName, reorganize, skipProviderIds, enabledProviders],
  );

  const pickFolder = async () => {
    try {
      const picked = await ImportService.PickFolder();
      if (picked) setRoot(picked);
    } catch (e) {
      toast.fromError(e, "Could not open folder picker");
    }
  };

  const analyze = async () => {
    if (!root) {
      toast.warn("Pick a source folder first");
      return;
    }
    if (mode === "copy" && !masterRoot) {
      toast.warn("No Master Library configured", "Set the Master Library root in Settings before copying.");
      return;
    }
    setStep(2);
    setAnalyzeRunning(true);
    setAnalyzeProgress(null);
    setDryRun(null);
    try {
      // Analyze runs server-side as a background job; progress and the finished
      // report arrive via import:progress / analyze:completed.
      await ImportService.StartAnalyze(buildOptions());
    } catch (e) {
      toast.fromError(e, "Could not start analysis");
      setAnalyzeRunning(false);
      setStep(1);
    }
  };

  // Cancel is read-only during analyze — no confirmation. Return to source,
  // preserving the folder/mode selection.
  const cancelAnalyze = async () => {
    setAnalyzeRunning(false);
    setAnalyzeProgress(null);
    setDryRun(null);
    setStep(1);
    try {
      await ImportService.CancelImport();
    } catch (e) {
      toast.fromError(e, "Could not cancel analysis");
    }
  };

  const startImport = async () => {
    setStarting(true);
    setCompleted(null);
    setProgress(null);
    try {
      await ImportService.StartImport(buildOptions());
      setStep(3);
    } catch (e) {
      toast.fromError(e, "Could not start import");
    } finally {
      setStarting(false);
    }
  };

  const resume = async (sessionID: string) => {
    try {
      await ImportService.ResumeSession(sessionID);
      setCompleted(null);
      setProgress(null);
      setStep(3);
      toast.info("Resuming import…");
    } catch (e) {
      toast.fromError(e, "Could not resume import");
    }
  };

  const confirmCancel = async () => {
    setCancelling(true);
    try {
      await ImportService.CancelImport();
      toast.info("Cancelling import…");
      setShowCancel(false);
    } catch (e) {
      toast.fromError(e, "Could not cancel import");
    } finally {
      setCancelling(false);
    }
  };

  const resetToStart = () => {
    setStep(1);
    setAnalyzeRunning(false);
    setAnalyzeProgress(null);
    setDryRun(null);
    setProgress(null);
    setCompleted(null);
  };

  if (booting) {
    return (
      <div>
        <PageHeader title="Import" />
        <LoadingBlock label="Preparing import…" />
      </div>
    );
  }

  return (
    <div>
      <PageHeader
        title="Import"
        description="Bring photos and videos into your archive — copy them in, or adopt an existing library in place."
      />

      {/* Interrupted sessions banner (idle only). */}
      {step === 1 && interrupted.length > 0 ? (
        <div className="mb-5 rounded-lg border border-amber-500/30 bg-amber-500/5 p-4">
          <div className="flex items-center gap-2">
            <ExclamationTriangleIcon className="h-5 w-5 text-amber-400" />
            <h3 className="text-sm font-semibold text-amber-200">
              {interrupted.length} interrupted import{interrupted.length > 1 ? "s" : ""}
            </h3>
          </div>
          <p className="mt-1 text-xs text-amber-200/70">
            These sessions stopped before finishing. Resuming re-scans and skips already-verified files — nothing is
            duplicated.
          </p>
          <ul className="mt-3 space-y-2">
            {interrupted.map((s) => (
              <li
                key={s.id}
                className="flex items-center justify-between gap-3 rounded-md border border-amber-500/20 bg-zinc-900/60 px-3 py-2"
              >
                <div className="min-w-0">
                  <div className="truncate text-[13px] text-zinc-200" title={s.destinationRoot}>
                    {baseName(s.destinationRoot) || s.destinationRoot || "Session"}
                  </div>
                  <div className="mt-0.5 flex items-center gap-2 text-[11px] text-zinc-500">
                    <StatusBadge status={s.status} />
                    <span>{formatNumber(s.filesImported)} imported so far</span>
                  </div>
                </div>
                <Button icon={ArrowPathIcon} variant="secondary" onClick={() => resume(s.id)}>
                  Resume
                </Button>
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      <Stepper step={step} />

      {step === 1 ? (
        <SourceStep
          root={root}
          mode={mode}
          reorganize={reorganize}
          eventName={eventName}
          masterRoot={masterRoot}
          metadataAvailable={settings?.metadataAvailable ?? true}
          providers={enabledProviders}
          skipProviderIds={skipProviderIds}
          onToggleProvider={(id, backUp) =>
            setSkipProviderIds((cur) => (backUp ? cur.filter((x) => x !== id) : cur.includes(id) ? cur : [...cur, id]))
          }
          onPick={pickFolder}
          onRootChange={setRoot}
          onModeChange={setMode}
          onReorganizeChange={setReorganize}
          onEventNameChange={setEventName}
          onAnalyze={analyze}
        />
      ) : null}

      {step === 2 ? (
        analyzeRunning ? (
          <AnalyzeProgress progress={analyzeProgress} onCancel={cancelAnalyze} />
        ) : (
          <DryRunStep
            report={dryRun}
            mode={mode}
            onBack={() => setStep(1)}
            onStart={startImport}
            starting={starting}
          />
        )
      ) : null}

      {step === 3 ? (
        <ImportStep
          progress={progress}
          completed={completed}
          mode={mode}
          root={root}
          skippedProviderNames={enabledProviders
            .filter((p) => skipProviderIds.includes(p.id))
            .map((p) => providerLabel(p))}
          onCancel={() => setShowCancel(true)}
          onNewImport={resetToStart}
          onViewHistory={() => navigate({ to: "/history" })}
          onResume={resume}
        />
      ) : null}

      <ConfirmDialog
        open={showCancel}
        title="Cancel this import?"
        description="Files already copied and verified are kept. The session is finalized as cancelled and can be resumed later."
        confirmLabel="Cancel import"
        cancelLabel="Keep importing"
        loading={cancelling}
        onConfirm={confirmCancel}
        onCancel={() => setShowCancel(false)}
      />
    </div>
  );
}

/* ------------------------------- Stepper -------------------------------- */

function Stepper({ step }: { step: Step }) {
  const steps: { n: Step; label: string }[] = [
    { n: 1, label: "Source" },
    { n: 2, label: "Dry run" },
    { n: 3, label: "Import" },
  ];
  return (
    <div className="mb-5 flex items-center gap-2">
      {steps.map((s, i) => {
        const active = step === s.n;
        const done = step > s.n;
        return (
          <div key={s.n} className="flex items-center gap-2">
            <div className="flex items-center gap-2">
              <span
                className={`flex h-6 w-6 items-center justify-center rounded-full text-[11px] font-semibold ${
                  active
                    ? "bg-blue-600 text-white"
                    : done
                      ? "bg-emerald-600/20 text-emerald-400"
                      : "bg-zinc-800 text-zinc-500"
                }`}
              >
                {done ? "✓" : s.n}
              </span>
              <span className={`text-[13px] ${active ? "font-medium text-zinc-100" : "text-zinc-500"}`}>
                {s.label}
              </span>
            </div>
            {i < steps.length - 1 ? <div className="h-px w-8 bg-zinc-800" /> : null}
          </div>
        );
      })}
    </div>
  );
}

/* ------------------------------ Step 1 ---------------------------------- */

interface SourceStepProps {
  root: string;
  mode: Mode;
  reorganize: boolean;
  eventName: string;
  masterRoot: string;
  metadataAvailable: boolean;
  providers: ProviderDTO[];
  skipProviderIds: string[];
  onToggleProvider: (id: string, backUp: boolean) => void;
  onPick: () => void;
  onRootChange: (v: string) => void;
  onModeChange: (m: Mode) => void;
  onReorganizeChange: (v: boolean) => void;
  onEventNameChange: (v: string) => void;
  onAnalyze: () => void;
}

// providerLabel derives a short human name for a backup destination from its
// config (localfs root / rclone remote), falling back to the plugin name.
function providerLabel(p: ProviderDTO): string {
  try {
    const cfg = JSON.parse(p.configJson || "{}") as Record<string, unknown>;
    if (typeof cfg.root === "string" && cfg.root) return baseName(cfg.root) || cfg.root;
    if (Array.isArray(cfg.remotes) && cfg.remotes.length > 0) return cfg.remotes.map(String).join(", ");
    if (typeof cfg.remote === "string" && cfg.remote) return cfg.remote;
  } catch {
    // fall through to plugin name
  }
  return p.pluginName;
}

function SourceStep(p: SourceStepProps) {
  const canAnalyze = !!p.root && (p.mode === "adopt" || !!p.masterRoot);
  return (
    <div className="space-y-4">
      <Card title="Source" subtitle="The folder or volume to import from.">
        <div className="flex flex-wrap items-center gap-3">
          <Button icon={FolderOpenIcon} variant="secondary" onClick={p.onPick}>
            Choose folder…
          </Button>
          <input
            value={p.root}
            onChange={(e) => p.onRootChange(e.target.value)}
            placeholder="No folder selected"
            className="min-w-0 flex-1 rounded-md border border-zinc-700 bg-zinc-950 px-3 py-1.5 font-mono text-[12px] text-zinc-200 outline-none focus:border-blue-500"
          />
        </div>
        {!p.metadataAvailable ? (
          <div className="mt-3 flex items-start gap-2 rounded-md border border-amber-500/30 bg-amber-500/5 p-2.5 text-[12px] text-amber-200/90">
            <ExclamationTriangleIcon className="mt-0.5 h-4 w-4 flex-none" />
            <span>
              exiftool was not detected. Imports will proceed with reduced metadata (capture date falls back to file
              modification time).
            </span>
          </div>
        ) : null}
      </Card>

      <Card title="Mode">
        <div className="grid gap-3 sm:grid-cols-2">
          <ModeOption
            active={p.mode === "copy"}
            title="Copy into archive"
            description="Copies files into your Master Library, verifies every byte, then records them as archived assets. Originals on the source are left untouched."
            onClick={() => p.onModeChange("copy")}
          />
          <ModeOption
            active={p.mode === "adopt"}
            title="Adopt in place (initialize)"
            description="Registers existing files where they already are — no copying, no extra storage. Each file is hashed as an integrity baseline and marked verified. Use this when the source is (or is becoming) your Master Library."
            onClick={() => p.onModeChange("adopt")}
          />
        </div>

        {p.mode === "adopt" ? (
          <label className="mt-3 flex cursor-pointer items-start gap-2.5 rounded-md border border-zinc-800 bg-zinc-950/40 p-3">
            <input
              type="checkbox"
              checked={p.reorganize}
              onChange={(e) => p.onReorganizeChange(e.target.checked)}
              className="mt-0.5 h-4 w-4 accent-blue-600"
            />
            <span className="text-[12px] text-zinc-300">
              <span className="font-medium text-zinc-200">Reorganize into archive layout</span> — move adopted files
              into the standard <span className="font-mono">YYYY/YYYY-MM-DD Event/</span> structure. Only same-drive
              atomic moves are performed; cross-volume files are left in place.
            </span>
          </label>
        ) : null}
      </Card>

      <Card title="Details">
        <label className="block">
          <span className="text-xs font-medium text-zinc-400">Event name (optional)</span>
          <input
            value={p.eventName}
            onChange={(e) => p.onEventNameChange(e.target.value)}
            placeholder="e.g. Iceland Trip"
            className="mt-1.5 w-full rounded-md border border-zinc-700 bg-zinc-950 px-3 py-1.5 text-[13px] text-zinc-200 outline-none focus:border-blue-500"
          />
          <span className="mt-1 block text-[11px] text-zinc-500">
            Used as the folder name inside each date directory. Leave empty for date-only folders.
          </span>
        </label>

        <div className="mt-4 rounded-md border border-zinc-800 bg-zinc-950/40 p-3">
          <div className="flex items-center gap-2 text-xs font-medium text-zinc-400">
            <InformationCircleIcon className="h-4 w-4" />
            Destination
          </div>
          {p.mode === "copy" ? (
            <p className="mt-1.5 font-mono text-[12px] text-zinc-300">
              {p.masterRoot || (
                <span className="font-sans text-amber-400">
                  No Master Library configured — set one in Settings first.
                </span>
              )}
            </p>
          ) : (
            <p className="mt-1.5 text-[12px] text-zinc-400">
              In adopt mode the destination is the source drive itself — files are registered where they already live.
              {p.reorganize ? " Reorganize is on, so they will be moved within that same drive." : ""}
            </p>
          )}
        </div>
      </Card>

      {p.providers.length > 0 ? (
        <Card title="Back up to" subtitle="Uncheck a destination to skip it for this import — useful when a card was already uploaded there.">
          <div className="space-y-2">
            {p.providers.map((prov) => {
              const backUp = !p.skipProviderIds.includes(prov.id);
              return (
                <label
                  key={prov.id}
                  className="flex cursor-pointer items-center gap-2.5 rounded-md border border-zinc-800 bg-zinc-950/40 px-3 py-2"
                >
                  <input
                    type="checkbox"
                    checked={backUp}
                    onChange={(e) => p.onToggleProvider(prov.id, e.target.checked)}
                    className="h-4 w-4 flex-none accent-blue-600"
                  />
                  <span className="min-w-0 flex-1 truncate text-[13px] text-zinc-200" title={providerLabel(prov)}>
                    {providerLabel(prov)}
                  </span>
                  {prov.mirror ? (
                    <span className="rounded-full bg-amber-500/15 px-2 py-0.5 text-[10px] font-semibold tracking-wide text-amber-300 uppercase ring-1 ring-amber-500/30 ring-inset">
                      Mirror
                    </span>
                  ) : null}
                  {!backUp ? <span className="text-[11px] text-zinc-500">Skipped</span> : null}
                </label>
              );
            })}
          </div>
          <p className="mt-2 text-[11px] text-zinc-500">
            Skipped destinations are recorded per asset (not just left un-queued), so safety checks still treat those
            assets as not backed up there. You can queue them later from Providers.
          </p>
        </Card>
      ) : null}

      <div className="flex justify-end">
        <Button icon={PlayIcon} variant="primary" size="lg" disabled={!canAnalyze} onClick={p.onAnalyze}>
          Analyze
        </Button>
      </div>
    </div>
  );
}

function ModeOption({
  active,
  title,
  description,
  onClick,
}: {
  active: boolean;
  title: string;
  description: string;
  onClick: () => void;
}) {
  return (
    <button
      onClick={onClick}
      className={`rounded-lg border p-3 text-left transition ${
        active ? "border-blue-500 bg-blue-600/10" : "border-zinc-800 bg-zinc-950/40 hover:border-zinc-700"
      }`}
    >
      <div className="flex items-center gap-2">
        <span
          className={`flex h-4 w-4 items-center justify-center rounded-full border ${
            active ? "border-blue-500" : "border-zinc-600"
          }`}
        >
          {active ? <span className="h-2 w-2 rounded-full bg-blue-500" /> : null}
        </span>
        <span className="text-[13px] font-medium text-zinc-100">{title}</span>
      </div>
      <p className="mt-1.5 text-[12px] leading-relaxed text-zinc-400">{description}</p>
    </button>
  );
}

/* ------------------------------ Step 2 ---------------------------------- */

const ANALYZE_PHASE_LABEL: Record<string, string> = {
  scanning: "Scanning",
  hashing: "Hashing",
  classifying: "Classifying",
};

// truncateMiddle shortens a string to max chars, keeping both ends and eliding
// the middle — so a long filename stays recognizable by its name and extension.
function truncateMiddle(s: string, max: number): string {
  if (s.length <= max) return s;
  const keep = max - 1;
  const head = Math.ceil(keep / 2);
  const tail = Math.floor(keep / 2);
  return `${s.slice(0, head)}…${s.slice(s.length - tail)}`;
}

// useTransferRate derives a bytes/sec rate from a rolling ~5s window of the
// observed bytesDone counter. Passing null (e.g. during scanning, where no byte
// total is known) clears the window and returns null.
function useTransferRate(bytesDone: number | null): number | null {
  const samplesRef = useRef<{ t: number; bytes: number }[]>([]);
  const [rate, setRate] = useState<number | null>(null);
  useEffect(() => {
    if (bytesDone == null) {
      samplesRef.current = [];
      setRate(null);
      return;
    }
    const now = Date.now();
    const samples = samplesRef.current;
    samples.push({ t: now, bytes: bytesDone });
    const cutoff = now - 5000;
    while (samples.length > 2 && samples[0].t < cutoff) samples.shift();
    const first = samples[0];
    const dt = (now - first.t) / 1000;
    const db = bytesDone - first.bytes;
    setRate(dt > 0.3 && db > 0 ? db / dt : null);
  }, [bytesDone]);
  return rate;
}

// AnalyzeProgress renders the running analyze sub-state of step 2: a large
// progress bar with verbose counters (files, bytes, rate, ETA), the current
// filename, and the phase label. The scanning phase shows an indeterminate bar
// with a live discovered-file count.
function AnalyzeProgress({ progress, onCancel }: { progress: ImportProgress | null; onCancel: () => void }) {
  const phase = progress?.phase || "scanning";
  const scanning = phase === "scanning";
  const bytesDone = progress?.bytesDone ?? 0;
  const bytesTotal = progress?.bytesTotal ?? 0;
  const rate = useTransferRate(scanning ? null : bytesDone);
  const percent = !scanning && bytesTotal > 0 ? (bytesDone / bytesTotal) * 100 : null;
  const etaSeconds = rate && rate > 0 && bytesTotal > bytesDone ? (bytesTotal - bytesDone) / rate : null;
  const phaseLabel = ANALYZE_PHASE_LABEL[phase] ?? titleCasePhase(phase);

  return (
    <div className="space-y-4">
      <div className="flex items-start gap-2 rounded-md border border-blue-500/30 bg-blue-500/5 p-3 text-[12px] text-blue-200/90">
        <InformationCircleIcon className="mt-0.5 h-4 w-4 flex-none" />
        <span>
          Analyzing — hashing every file to predict the import. Nothing is modified. You can leave this page and come
          back; the analysis keeps running in the background.
        </span>
      </div>

      <Card>
        <div className="mb-4 flex items-center justify-between">
          <div className="flex items-center gap-2">
            <ArrowPathIcon className="h-4 w-4 animate-spin text-blue-400" />
            <span className="text-sm font-medium text-zinc-200">Analyzing…</span>
            <StatusBadge status={phase} tone="info" label={phaseLabel} />
          </div>
          <Button icon={StopIcon} variant="danger" onClick={onCancel}>
            Cancel
          </Button>
        </div>

        {scanning ? (
          <ProgressBar
            percent={null}
            size="lg"
            showPercent={false}
            label={`Discovered ${formatNumber(progress?.filesDone ?? 0)} files…`}
          />
        ) : (
          <ProgressBar
            percent={percent}
            striped
            size="lg"
            label={progress?.currentFile ? truncateMiddle(baseName(progress.currentFile), 52) : "Working…"}
            detail={`${formatBytes(bytesDone)} of ${formatBytes(bytesTotal)} · ${formatRate(rate)}`}
          />
        )}

        <div className="mt-4 grid grid-cols-2 gap-3 sm:grid-cols-4">
          <CounterCard
            label="Files"
            value={progress?.filesDone ?? 0}
            hint={
              progress && progress.filesTotal > 0
                ? `of ${formatNumber(progress.filesTotal)}`
                : scanning
                  ? "discovering…"
                  : undefined
            }
          />
          <CounterCard label="Data" valueText={formatBytes(bytesDone)} hint={bytesTotal > 0 ? `of ${formatBytes(bytesTotal)}` : undefined} />
          <CounterCard label="Rate" valueText={scanning ? "—" : formatRate(rate)} tone="accent" />
          <CounterCard label="Time left" valueText={scanning ? "—" : formatEta(etaSeconds)} />
        </div>

        {progress?.currentFile ? (
          <p className="selectable mt-3 truncate font-mono text-[11px] text-zinc-500" title={progress.currentFile}>
            {progress.currentFile}
          </p>
        ) : null}
      </Card>
    </div>
  );
}

function DryRunStep({
  report,
  mode,
  onBack,
  onStart,
  starting,
}: {
  report: DryRunReportDTO | null;
  mode: Mode;
  onBack: () => void;
  onStart: () => void;
  starting: boolean;
}) {
  return (
    <div className="space-y-4">
      <div className="flex items-start gap-2 rounded-md border border-blue-500/30 bg-blue-500/5 p-3 text-[12px] text-blue-200/90">
        <InformationCircleIcon className="mt-0.5 h-4 w-4 flex-none" />
        <span>
          This is a dry run — nothing has been modified. Review the plan below, then start the import when you are
          ready.
        </span>
      </div>

      {report ? (
        <>
          <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
            <SummaryCard label="Files" value={formatNumber(report.files)} icon={Squares2X2Icon} />
            <SummaryCard label="Photos" value={formatNumber(report.photos)} icon={PhotoIcon} />
            <SummaryCard label="Videos" value={formatNumber(report.videos)} icon={FilmIcon} />
            <SummaryCard label="Already Imported" value={formatNumber(report.alreadyImported)} />
            <SummaryCard
              label="Duplicates"
              value={formatNumber(report.duplicates)}
              icon={DocumentDuplicateIcon}
              tone={report.duplicates > 0 ? "warn" : "default"}
            />
            <SummaryCard label="New" value={formatNumber(report.new)} tone="success" />
            <SummaryCard label="Import Size" value={formatBytes(report.totalImportBytes)} />
            <SummaryCard label="Estimated Time" value={formatEta(report.estimatedSeconds)} />
          </div>

          {mode === "adopt" ? (
            <Card title="Adopt plan">
              <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
                <SummaryCard label="Will adopt in place" value={formatNumber(report.plannedAdoptions)} />
                <SummaryCard
                  label="Planned moves"
                  value={formatNumber(report.plannedMoves)}
                  hint={report.reorganize ? "Same-drive reorganize" : "Reorganize off"}
                />
                <SummaryCard label="Already archived" value={formatNumber(report.alreadyImported)} />
              </div>
            </Card>
          ) : null}

          <div className="flex items-center justify-between">
            <Button icon={ArrowLeftIcon} variant="ghost" onClick={onBack}>
              Back
            </Button>
            <Button
              icon={PlayIcon}
              variant="primary"
              size="lg"
              onClick={onStart}
              loading={starting}
              disabled={report.new + report.duplicates + report.plannedAdoptions === 0}
            >
              Start import
            </Button>
          </div>
        </>
      ) : (
        <Card>
          <div className="py-6 text-center text-sm text-zinc-500">No analysis available.</div>
          <div className="flex justify-center">
            <Button icon={ArrowLeftIcon} variant="secondary" onClick={onBack}>
              Back to source
            </Button>
          </div>
        </Card>
      )}
    </div>
  );
}

function SummaryCard({
  label,
  value,
  hint,
  icon: Icon,
  tone = "default",
}: {
  label: string;
  value: string;
  hint?: string;
  icon?: React.ComponentType<{ className?: string }>;
  tone?: "default" | "success" | "warn";
}) {
  const color = tone === "success" ? "text-emerald-400" : tone === "warn" ? "text-amber-400" : "text-zinc-100";
  return (
    <div className="rounded-lg border border-zinc-800 bg-zinc-900/60 p-3">
      <div className="flex items-center justify-between">
        <span className="text-[11px] font-medium text-zinc-500">{label}</span>
        {Icon ? <Icon className="h-4 w-4 text-zinc-600" /> : null}
      </div>
      <div className={`mt-1.5 text-xl font-semibold tabular-nums ${color}`}>{value}</div>
      {hint ? <div className="mt-0.5 text-[10px] text-zinc-500">{hint}</div> : null}
    </div>
  );
}

/* ------------------------------ Step 3 ---------------------------------- */

function ImportStep({
  progress,
  completed,
  mode,
  root,
  skippedProviderNames,
  onCancel,
  onNewImport,
  onViewHistory,
  onResume,
}: {
  progress: ImportProgress | null;
  completed: ImportCompleted | null;
  mode: Mode;
  root: string;
  skippedProviderNames: string[];
  onCancel: () => void;
  onNewImport: () => void;
  onViewHistory: () => void;
  onResume: (sessionID: string) => void;
}) {
  if (completed) {
    const failed = completed.status === "failed";
    const cancelled = completed.status === "cancelled";
    const interrupted = completed.status === "interrupted";
    return (
      <Card>
        <div className="flex flex-col items-center gap-3 py-4 text-center">
          {failed ? (
            <ExclamationTriangleIcon className="h-10 w-10 text-red-400" />
          ) : interrupted ? (
            <ExclamationTriangleIcon className="h-10 w-10 text-amber-400" />
          ) : cancelled ? (
            <StopIcon className="h-10 w-10 text-zinc-400" />
          ) : (
            <CheckCircleIcon className="h-10 w-10 text-emerald-400" />
          )}
          <div>
            <h3 className="text-base font-semibold text-zinc-100">
              {failed
                ? "Import failed"
                : interrupted
                  ? "Import interrupted"
                  : cancelled
                    ? "Import cancelled"
                    : "Import complete"}
            </h3>
            <div className="mt-1 flex items-center justify-center gap-2">
              <StatusBadge status={completed.status} />
            </div>
            {interrupted ? (
              <p className="mx-auto mt-2 max-w-md text-[12px] text-amber-200/80">
                The import stopped before finishing — the destination may have run out of space or been disconnected.
                The counters below are preserved. Resuming re-scans and skips already-verified files, so nothing is
                duplicated.
              </p>
            ) : null}
            {failed ? (
              <p className="mx-auto mt-2 max-w-md text-[12px] text-red-300/80">
                No files were imported and one or more files failed. Check the source and destination, then try the
                import again.
              </p>
            ) : null}
          </div>
        </div>

        <div className="grid grid-cols-2 gap-3 sm:grid-cols-5">
          <CounterCard label="Scanned" value={completed.filesScanned} />
          <CounterCard label="Imported" value={completed.filesImported} tone="success" />
          <CounterCard label="Duplicates" value={completed.duplicates} tone={completed.duplicates > 0 ? "warn" : "default"} />
          <CounterCard label="Failures" value={completed.failures} tone={completed.failures > 0 ? "danger" : "default"} />
          <CounterCard label="Skipped" value={completed.skipped} />
        </div>

        {completed.failures > 0 ? (
          <div className="mt-4">
            <FailedFilesPanel sessionId={completed.sessionId} sessionFailures={completed.failures} />
          </div>
        ) : null}

        <div className="mt-5 flex items-center justify-center gap-2">
          {interrupted ? (
            <Button icon={ArrowPathIcon} variant="primary" onClick={() => onResume(completed.sessionId)}>
              Resume
            </Button>
          ) : null}
          <Button variant="secondary" onClick={onNewImport}>
            Start another import
          </Button>
          <Button variant={interrupted ? "ghost" : "primary"} onClick={onViewHistory}>
            View in history
          </Button>
        </div>
        <p className="mt-3 text-center text-[11px] text-zinc-500">
          Backups run asynchronously from here.{" "}
          <Link to="/backup-queue" className="text-blue-400 hover:text-blue-300">
            Open the backup queue
          </Link>
          .
        </p>
        {skippedProviderNames.length > 0 ? (
          <p className="mt-1 text-center text-[11px] text-amber-300/80">
            Skipped by choice: {skippedProviderNames.join(", ")}. These assets are recorded as opted out there — queue
            them anytime from Providers.
          </p>
        ) : null}

        {/* Clear-after-import: copy mode only, with a real source root, and only
            once the import itself succeeded. Adopt mode has no source to clear —
            the source IS the library. */}
        {mode === "copy" && !failed && !cancelled && !interrupted && root ? (
          <ClearTheSourceSection root={root} sessionId={completed.sessionId} />
        ) : null}
      </Card>
    );
  }

  const pct = progress?.percent ?? null;
  const phase = progress?.phase || "starting";
  const phaseLabel = IMPORT_PHASE_LABEL[phase] ?? titleCasePhase(phase);

  // Verbose per-phase bar label. The metadata phase (which on huge libraries can
  // run for a long time before the copy loop begins) gets an explicit
  // "Reading metadata · N of M files"; the generic "Preparing…" fallback now only
  // shows for the brief startup/preparing moments with no file context yet.
  const barLabel =
    phase === "extracting-metadata"
      ? `Reading metadata · ${formatNumber(progress?.filesDone ?? 0)} of ${formatNumber(progress?.filesTotal ?? 0)} files`
      : phase === "preparing"
        ? "Preparing archive…"
        : progress?.currentFile
          ? baseName(progress.currentFile)
          : "Preparing…";

  return (
    <Card>
      <div className="mb-4 flex items-center justify-between">
        <div className="flex items-center gap-2">
          <ArrowPathIcon className="h-4 w-4 animate-spin text-blue-400" />
          <span className="text-sm font-medium text-zinc-200">Importing…</span>
          <StatusBadge status={phase} tone="info" label={phaseLabel} />
        </div>
        <Button icon={StopIcon} variant="danger" onClick={onCancel}>
          Cancel
        </Button>
      </div>

      <ProgressBar
        percent={phase === "preparing" ? null : pct}
        striped
        size="lg"
        label={barLabel}
        detail={
          progress && progress.filesTotal > 0
            ? `${formatNumber(progress.filesDone)} / ${formatNumber(progress.filesTotal)} files`
            : undefined
        }
      />

      <div className="mt-4 grid grid-cols-2 gap-3 sm:grid-cols-4">
        <CounterCard label="Files" value={progress?.filesDone ?? 0} hint={`of ${formatNumber(progress?.filesTotal ?? 0)}`} />
        <CounterCard
          label="Data"
          valueText={formatBytes(progress?.bytesDone ?? 0)}
          hint={`of ${formatBytes(progress?.bytesTotal ?? 0)}`}
        />
        <CounterCard label="Errors" value={progress?.errors ?? 0} tone={(progress?.errors ?? 0) > 0 ? "danger" : "default"} />
        <CounterCard label="Progress" valueText={pct != null ? `${pct >= 100 ? 100 : Math.floor(pct)}%` : "—"} tone="accent" />
      </div>

      {progress?.currentFile ? (
        <p className="selectable mt-3 truncate font-mono text-[11px] text-zinc-500" title={progress.currentFile}>
          {progress.currentFile}
        </p>
      ) : null}
    </Card>
  );
}

/* -------------------------- Clear the source? --------------------------- */

/**
 * ClearTheSourceSection closes the import loop on a completed copy-mode import.
 * It explains the gate ("backups must finish first"), shows live per-session
 * backup progress, offers "Evaluate now" (reusing the safe-to-erase background
 * job for the source root), and — only on a fresh green evaluation — the gated
 * "Clear imported media…" affordance.
 *
 * When the per-session backup status reaches complete AND the last evaluation was
 * green-except-for-backups, it re-runs the (now cheap, catalog-informed) safe-to-
 * erase evaluation automatically so the user does not have to click Evaluate
 * twice.
 */
function ClearTheSourceSection({ root, sessionId }: { root: string; sessionId: string }) {
  const toast = useToast();
  const [backup, setBackup] = useState<SessionBackupStatusDTO | null>(null);
  const [report, setReport] = useState<SafeToEraseDTO | null>(null);
  const [evalProgress, setEvalProgress] = useState<SourceProgress | null>(null);
  const [evalRunning, setEvalRunning] = useState(false);
  const [evalError, setEvalError] = useState("");

  const refreshBackup = useCallback(() => {
    void BackupService.SessionBackupStatus(sessionId)
      .then(setBackup)
      .catch(() => undefined);
  }, [sessionId]);

  const evaluate = useCallback(async () => {
    setEvalError("");
    try {
      await SourcesService.StartSafeToErase("", root);
    } catch (e) {
      toast.fromError(e, "Could not start evaluation");
    }
  }, [root, toast]);

  // Load backup status + re-attach any running/completed evaluation for this root.
  useEffect(() => {
    refreshBackup();
    void SourcesService.ActiveSafeToErase()
      .then((dto) => {
        if (dto.mountPoint !== root) return;
        if (dto.state === "running") {
          setEvalRunning(true);
          setEvalProgress(dto.progress);
        } else if (dto.state === "completed" && dto.report) {
          setReport(dto.report);
        }
      })
      .catch(() => undefined);
  }, [root, refreshBackup]);

  // Poll backup status; also refresh promptly on any queue change.
  useEffect(() => {
    const id = setInterval(refreshBackup, 4000);
    return () => clearInterval(id);
  }, [refreshBackup]);
  useWailsEvent(WailsEvents.BackupQueueChanged, () => refreshBackup());

  useWailsEvent<SourceProgress>(WailsEvents.SourceProgress, (p) => {
    if (p.kind === "safe-to-erase" && p.mountPoint === root) {
      setEvalRunning(true);
      setEvalProgress(p);
    }
  });
  useWailsEvent<SourceEvaluated>(WailsEvents.SourceEvaluated, (e) => {
    if (e.mountPoint !== root) return;
    setEvalRunning(false);
    setEvalProgress(null);
    setReport(e.report);
    setEvalError(e.error);
  });

  // Auto-refresh: once backups finish, if the only thing that had kept the source
  // from being safe was incomplete backups, re-run the (now cheap) evaluation.
  const backupComplete = !!backup?.complete && (backup?.totalAssets ?? 0) > 0;
  const greenExceptBackups =
    !!report && !report.safe && report.new === 0 && report.unverified === 0 && report.backupIncomplete > 0;
  useEffect(() => {
    if (backupComplete && greenExceptBackups && !evalRunning) {
      void evaluate();
    }
    // Only react to the backup-completion transition; report/evalRunning are read
    // fresh above.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [backupComplete]);

  const cancelEvaluate = () => void SourcesService.CancelSafeToErase().catch(() => undefined);

  const backedUp = backup?.backedUp ?? 0;
  const totalAssets = backup?.totalAssets ?? 0;
  const backupPct = totalAssets > 0 ? Math.min(100, Math.round((backedUp / totalAssets) * 100)) : 0;

  return (
    <div className="mt-5 rounded-lg border border-zinc-800 bg-zinc-950/40 p-4">
      <div className="mb-1 flex items-center gap-2">
        <ShieldCheckIcon className="h-4 w-4 text-zinc-400" />
        <h4 className="text-sm font-semibold text-zinc-200">Clear the source?</h4>
      </div>
      <p className="text-[12px] leading-relaxed text-zinc-500">
        Once every imported file is verified and fully backed up, PAIM can move the originals off the card into a trash
        folder on the card (it never deletes). Backups must finish first.
      </p>

      {/* Live per-session backup status. */}
      <div className="mt-3">
        <div className="mb-1 flex items-center justify-between text-[11px]">
          <span className="text-zinc-500">Backups for this import</span>
          <span className="tabular-nums text-zinc-400">
            {formatNumber(backedUp)} of {formatNumber(totalAssets)} backed up
          </span>
        </div>
        <div className="h-1.5 w-full overflow-hidden rounded-full bg-zinc-800">
          <div
            className={`h-full rounded-full ${backupComplete ? "bg-emerald-500" : "bg-blue-500"} transition-all`}
            style={{ width: `${backupPct}%` }}
          />
        </div>
        {totalAssets === 0 ? (
          <p className="mt-1 text-[10px] text-zinc-600">No backup jobs for this import (no backup destination configured).</p>
        ) : null}
      </div>

      {/* Evaluate + result. */}
      <div className="mt-3 flex flex-wrap items-center gap-2">
        <Button icon={ShieldCheckIcon} variant="secondary" onClick={() => void evaluate()} loading={evalRunning} disabled={evalRunning}>
          {report ? "Re-evaluate" : "Evaluate now"}
        </Button>
        {evalRunning ? (
          <span className="flex items-center gap-2 text-[11px] text-zinc-500">
            <span className="tabular-nums">
              {formatNumber(evalProgress?.filesDone ?? 0)} of {formatNumber(evalProgress?.filesTotal ?? 0)} checked
            </span>
            <button className="text-blue-400 hover:underline" onClick={cancelEvaluate}>
              Cancel
            </button>
          </span>
        ) : null}
      </div>

      {evalError ? <p className="mt-2 text-[11px] text-red-400">Evaluation failed: {evalError}</p> : null}
      {report ? (
        <p className={`mt-2 text-[12px] leading-relaxed ${report.safe ? "text-emerald-400" : "text-zinc-400"}`}>
          {report.reason}
          {report.fastPath + report.hashed > 0 ? (
            <span className="text-zinc-600">
              {" "}
              ({formatNumber(report.fastPath)} from catalog · {formatNumber(report.hashed)} re-hashed)
            </span>
          ) : null}
        </p>
      ) : null}

      <ClearSourceControl root={root} report={report} fresh={!!report?.safe && !evalRunning} />
    </div>
  );
}

// Friendly badge labels for the import phases (falls back to titleCasePhase).
const IMPORT_PHASE_LABEL: Record<string, string> = {
  preparing: "Preparing",
  scanning: "Scanning",
  "extracting-metadata": "Reading metadata",
  importing: "Importing",
  reorganizing: "Reorganizing",
  done: "Done",
};

function titleCasePhase(phase: string): string {
  return phase.replace(/[_-]+/g, " ").replace(/\b\w/g, (c) => c.toUpperCase());
}

function CounterCard({
  label,
  value,
  valueText,
  hint,
  tone = "default",
}: {
  label: string;
  value?: number;
  valueText?: string;
  hint?: string;
  tone?: "default" | "success" | "warn" | "danger" | "accent";
}) {
  const color =
    tone === "success"
      ? "text-emerald-400"
      : tone === "warn"
        ? "text-amber-400"
        : tone === "danger"
          ? "text-red-400"
          : tone === "accent"
            ? "text-blue-400"
            : "text-zinc-100";
  return (
    <div className="rounded-lg border border-zinc-800 bg-zinc-900/60 p-3 text-center">
      <div className={`text-lg font-semibold tabular-nums ${color}`}>
        {valueText ?? formatNumber(value ?? 0)}
      </div>
      <div className="mt-0.5 text-[11px] text-zinc-500">{label}</div>
      {hint ? <div className="text-[10px] text-zinc-600">{hint}</div> : null}
    </div>
  );
}
