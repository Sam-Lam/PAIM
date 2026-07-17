import { useCallback, useEffect, useState } from "react";
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
  Squares2X2Icon,
  StopIcon,
} from "@heroicons/react/24/outline";
import { Button, Card, ConfirmDialog, LoadingBlock, PageHeader, ProgressBar, StatusBadge } from "../components";
import {
  HistoryService,
  ImportOptions,
  ImportService,
  SettingsService,
  WailsEvents,
  type DryRunReportDTO,
  type ImportCompleted,
  type ImportProgress,
  type ScanSummary,
  type SessionDTO,
  type Settings,
} from "../lib/api";
import { useWailsEvent } from "../lib/hooks";
import { useToast } from "../lib/toast";
import { baseName, formatBytes, formatEta, formatNumber } from "../lib/format";

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

  // Step 2 — analysis.
  const [analyzing, setAnalyzing] = useState(false);
  const [scan, setScan] = useState<ScanSummary | null>(null);
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
  useWailsEvent<ImportProgress>(WailsEvents.ImportProgress, (data) => {
    setProgress(data);
  });
  useWailsEvent<ImportCompleted>(WailsEvents.ImportCompleted, (data) => {
    setCompleted(data);
    void refreshInterrupted();
  });

  const refreshInterrupted = useCallback(async () => {
    try {
      const page = await HistoryService.ListSessions(1, 50);
      setInterrupted((page.items ?? []).filter((s) => s.status === "interrupted"));
    } catch {
      // non-fatal
    }
  }, []);

  // On mount: load settings, detect an already-running import, seed the picked
  // root from ?root=, and surface interrupted sessions.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const [s, active] = await Promise.all([SettingsService.GetAll(), ImportService.ActiveImport()]);
        if (cancelled) return;
        setSettings(s);
        if (s.defaultEventName) setEventName(s.defaultEventName);
        if (active) {
          // App restarted / resumed elsewhere mid-import: attach at step 3.
          setProgress(active);
          setStep(3);
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
      }) satisfies ImportOptions,
    [root, mode, masterRoot, eventName, reorganize],
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
    setAnalyzing(true);
    setScan(null);
    setDryRun(null);
    try {
      const opts = buildOptions();
      const scanResult = await ImportService.ScanSource(root);
      setScan(scanResult);
      const report = await ImportService.DryRun(root, opts);
      setDryRun(report);
    } catch (e) {
      toast.fromError(e, "Analysis failed");
    } finally {
      setAnalyzing(false);
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
    setScan(null);
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
          onPick={pickFolder}
          onRootChange={setRoot}
          onModeChange={setMode}
          onReorganizeChange={setReorganize}
          onEventNameChange={setEventName}
          onAnalyze={analyze}
        />
      ) : null}

      {step === 2 ? (
        <DryRunStep
          analyzing={analyzing}
          scan={scan}
          report={dryRun}
          mode={mode}
          onBack={() => setStep(1)}
          onStart={startImport}
          starting={starting}
        />
      ) : null}

      {step === 3 ? (
        <ImportStep
          progress={progress}
          completed={completed}
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
  onPick: () => void;
  onRootChange: (v: string) => void;
  onModeChange: (m: Mode) => void;
  onReorganizeChange: (v: boolean) => void;
  onEventNameChange: (v: string) => void;
  onAnalyze: () => void;
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

function DryRunStep({
  analyzing,
  scan,
  report,
  mode,
  onBack,
  onStart,
  starting,
}: {
  analyzing: boolean;
  scan: ScanSummary | null;
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

      {analyzing ? (
        <Card>
          <LoadingBlock label={scan ? "Hashing and classifying files…" : "Scanning source…"} />
        </Card>
      ) : report ? (
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
  onCancel,
  onNewImport,
  onViewHistory,
  onResume,
}: {
  progress: ImportProgress | null;
  completed: ImportCompleted | null;
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
      </Card>
    );
  }

  const pct = progress?.percent ?? null;
  const phase = progress?.phase || "starting";

  return (
    <Card>
      <div className="mb-4 flex items-center justify-between">
        <div className="flex items-center gap-2">
          <ArrowPathIcon className="h-4 w-4 animate-spin text-blue-400" />
          <span className="text-sm font-medium text-zinc-200">Importing…</span>
          <StatusBadge status={phase} tone="info" label={titleCasePhase(phase)} />
        </div>
        <Button icon={StopIcon} variant="danger" onClick={onCancel}>
          Cancel
        </Button>
      </div>

      <ProgressBar
        percent={pct}
        striped
        size="lg"
        label={progress?.currentFile ? baseName(progress.currentFile) : "Preparing…"}
        detail={
          progress
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
        <CounterCard label="Progress" valueText={pct != null ? `${Math.round(pct)}%` : "—"} tone="accent" />
      </div>

      {progress?.currentFile ? (
        <p className="selectable mt-3 truncate font-mono text-[11px] text-zinc-500" title={progress.currentFile}>
          {progress.currentFile}
        </p>
      ) : null}
    </Card>
  );
}

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
