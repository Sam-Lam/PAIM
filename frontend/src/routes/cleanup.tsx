import { useEffect, useState } from "react";
import {
  ArchiveBoxIcon,
  ChevronDownIcon,
  ChevronRightIcon,
  DocumentDuplicateIcon,
  ExclamationTriangleIcon,
  FolderOpenIcon,
  LockClosedIcon,
  MagnifyingGlassIcon,
  QuestionMarkCircleIcon,
  ShieldCheckIcon,
  SparklesIcon,
  XCircleIcon,
} from "@heroicons/react/24/outline";
import { Button, Card, PageHeader } from "../components";
import {
  CleanupService,
  WailsEvents,
  type ClassStatDTO,
  type CleanupCompleted,
  type CleanupProgress,
  type CleanupReportDTO,
} from "../lib/api";
import { useWailsEvent } from "../lib/hooks";
import { useToast } from "../lib/toast";
import { baseName, formatBytes, formatNumber } from "../lib/format";

const MAX_ROWS = 500;

interface ClassMeta {
  label: string;
  icon: React.ComponentType<{ className?: string }>;
  accent: string;
  ring: string;
}

const CLASS_META: Record<string, ClassMeta> = {
  already_archived: { label: "Already Archived", icon: ArchiveBoxIcon, accent: "text-emerald-400", ring: "border-emerald-500/25" },
  duplicate: { label: "Duplicates", icon: DocumentDuplicateIcon, accent: "text-amber-400", ring: "border-amber-500/25" },
  new: { label: "New", icon: SparklesIcon, accent: "text-blue-400", ring: "border-blue-500/25" },
  unknown: { label: "Unknown", icon: QuestionMarkCircleIcon, accent: "text-zinc-400", ring: "border-zinc-700/50" },
  verification_failed: { label: "Verification Failed", icon: XCircleIcon, accent: "text-red-400", ring: "border-red-500/25" },
};

function metaFor(cls: string): ClassMeta {
  return (
    CLASS_META[cls] ?? {
      label: cls.replace(/[_-]+/g, " ").replace(/\b\w/g, (c) => c.toUpperCase()),
      icon: QuestionMarkCircleIcon,
      accent: "text-zinc-400",
      ring: "border-zinc-700/50",
    }
  );
}

/** Cleanup Assistant — strictly read-only folder analysis + safe-delete recommendation. */
export function CleanupPage() {
  const toast = useToast();
  const [root, setRoot] = useState("");
  const [analyzing, setAnalyzing] = useState(false);
  const [progress, setProgress] = useState<CleanupProgress | null>(null);
  const [report, setReport] = useState<CleanupReportDTO | null>(null);

  // Re-attach to a running/completed analysis on mount so navigating away and
  // back never loses a long analysis or its cached report (15-minute TTL).
  useEffect(() => {
    void CleanupService.ActiveCleanupAnalyze()
      .then((dto) => {
        if (dto.state === "running") {
          setAnalyzing(true);
          setRoot(dto.root);
          setProgress(dto.progress ?? null);
        } else if (dto.state === "completed" && dto.report) {
          setReport(dto.report);
          setRoot(dto.root);
        }
      })
      .catch(() => undefined);
  }, []);

  useWailsEvent<CleanupProgress>(WailsEvents.CleanupProgress, (p) => {
    setAnalyzing(true);
    setProgress(p);
  });
  useWailsEvent<CleanupCompleted>(WailsEvents.CleanupCompleted, (c) => {
    setAnalyzing(false);
    setProgress(null);
    if (c.error) {
      toast.error(`Analysis failed: ${c.error}`);
    } else if (c.cancelled) {
      toast.info("Analysis cancelled");
    } else if (c.report) {
      setReport(c.report);
    }
  });

  const pick = async () => {
    try {
      const picked = await CleanupService.PickFolder();
      if (picked) setRoot(picked);
    } catch (e) {
      toast.fromError(e, "Could not open folder picker");
    }
  };

  const analyze = async () => {
    if (!root) {
      toast.warn("Pick a folder to analyze first");
      return;
    }
    setAnalyzing(true);
    setProgress(null);
    setReport(null);
    try {
      await CleanupService.StartCleanupAnalyze(root);
    } catch (e) {
      setAnalyzing(false);
      toast.fromError(e, "Analysis failed");
    }
  };

  const cancel = () => {
    void CleanupService.CancelCleanupAnalyze().catch(() => undefined);
  };

  const anyTruncated = report?.classes?.some((c) => c.truncated) ?? false;

  return (
    <div>
      <PageHeader
        title="Cleanup Assistant"
        description="Analyze any folder against your archive and get a safe-to-delete recommendation with the reasons. Analysis is strictly read-only."
      />

      <Card className="mb-5">
        <div className="mb-3 flex items-start gap-2 rounded-md border border-blue-500/30 bg-blue-500/5 p-2.5 text-[12px] text-blue-200/90">
          <LockClosedIcon className="mt-0.5 h-4 w-4 flex-none" />
          <span>
            PAIM only reads and classifies these files — it never moves, modifies, or deletes anything here. Any actual
            deletion is up to you, in Finder.
          </span>
        </div>
        <div className="flex flex-wrap items-center gap-3">
          <Button icon={FolderOpenIcon} variant="secondary" onClick={pick}>
            Choose folder…
          </Button>
          <input
            value={root}
            onChange={(e) => setRoot(e.target.value)}
            placeholder="No folder selected"
            className="min-w-0 flex-1 rounded-md border border-zinc-700 bg-zinc-950 px-3 py-1.5 font-mono text-[12px] text-zinc-200 outline-none focus:border-blue-500"
          />
          <Button icon={MagnifyingGlassIcon} variant="primary" onClick={analyze} disabled={!root} loading={analyzing}>
            Analyze
          </Button>
        </div>
      </Card>

      {analyzing ? (
        <Card>
          <div className="flex items-center justify-between">
            <div className="min-w-0">
              <p className="text-sm font-medium text-zinc-200">
                Read-only analysis — hashing and classifying every media file…
              </p>
              <p className="mt-1 text-[12px] text-zinc-500 tabular-nums">
                {formatNumber(progress?.filesDone ?? 0)} files checked
              </p>
              {progress?.currentFile ? (
                <p className="selectable mt-1 truncate font-mono text-[11px] text-zinc-600" title={progress.currentFile}>
                  {progress.currentFile}
                </p>
              ) : null}
            </div>
            <Button variant="secondary" onClick={cancel}>
              Cancel
            </Button>
          </div>
          {/* Indeterminate: the total is unknown during the single-pass walk. */}
          <div className="mt-3 h-1.5 w-full overflow-hidden rounded-full bg-zinc-800">
            <div className="h-full w-1/3 animate-pulse rounded-full bg-blue-500" />
          </div>
        </Card>
      ) : report ? (
        <div className="space-y-5">
          <ClassGrid classes={report.classes ?? []} />

          <Recommendation report={report} />

          {anyTruncated ? (
            <div className="flex items-start gap-2 rounded-md border border-amber-500/30 bg-amber-500/5 p-3 text-[12px] text-amber-200/90">
              <ExclamationTriangleIcon className="mt-0.5 h-4 w-4 flex-none" />
              <span>
                This folder is very large — results were capped and show the first 10,000 files per class. Counts and the
                recommendation reflect the sampled set.
              </span>
            </div>
          ) : null}

          <div>
            <h2 className="mb-2 text-xs font-semibold tracking-wide text-zinc-500 uppercase">Files by classification</h2>
            <div className="space-y-2">
              {(report.classes ?? [])
                .filter((c) => c.count > 0)
                .map((c) => (
                  <ClassFileList key={c.class} stat={c} />
                ))}
            </div>
          </div>

          <p className="text-center text-[11px] text-zinc-600">
            Analyzed <span className="font-mono text-zinc-500">{report.root}</span> · {formatNumber(report.totalFiles)}{" "}
            files · {formatNumber(report.mediaFiles)} media · nothing was modified.
          </p>
        </div>
      ) : null}
    </div>
  );
}

function ClassGrid({ classes }: { classes: ClassStatDTO[] }) {
  return (
    <div className="grid grid-cols-2 gap-3 lg:grid-cols-5">
      {classes.map((c) => {
        const meta = metaFor(c.class);
        const Icon = meta.icon;
        return (
          <div key={c.class} className={`rounded-lg border bg-zinc-900/60 p-4 ${meta.ring}`}>
            <div className="flex items-center justify-between">
              <span className="text-[11px] font-medium text-zinc-500">{meta.label}</span>
              <Icon className={`h-4 w-4 ${meta.accent}`} />
            </div>
            <div className={`mt-2 text-2xl font-semibold tabular-nums ${meta.accent}`}>{formatNumber(c.count)}</div>
            <div className="mt-0.5 text-[11px] text-zinc-500">{formatBytes(c.bytes)}</div>
          </div>
        );
      })}
    </div>
  );
}

function Recommendation({ report }: { report: CleanupReportDTO }) {
  const rec = report.recommendation;
  const safe = rec.safeToDelete;
  return (
    <div
      className={`rounded-xl border p-5 ${
        safe ? "border-emerald-500/40 bg-emerald-500/[0.06]" : "border-amber-500/40 bg-amber-500/[0.06]"
      }`}
    >
      <div className="flex items-start gap-3">
        <div
          className={`flex-none rounded-full p-2 ${safe ? "bg-emerald-500/15" : "bg-amber-500/15"}`}
        >
          {safe ? (
            <ShieldCheckIcon className="h-6 w-6 text-emerald-400" />
          ) : (
            <ExclamationTriangleIcon className="h-6 w-6 text-amber-400" />
          )}
        </div>
        <div className="min-w-0 flex-1">
          <h3 className={`text-base font-semibold ${safe ? "text-emerald-300" : "text-amber-200"}`}>
            {rec.title || (safe ? "Safe to Delete" : "Deletion NOT Recommended")}
          </h3>
          {rec.summary ? (
            <p className={`mt-1 text-[13px] leading-relaxed ${safe ? "text-emerald-200/80" : "text-amber-200/80"}`}>
              {rec.summary}
            </p>
          ) : null}

          {!safe && rec.reasons && rec.reasons.length > 0 ? (
            <div className="mt-3">
              <p className="mb-1 text-[11px] font-medium text-amber-300/80">Reasons</p>
              <ul className="space-y-1">
                {(rec.reasons ?? []).map((r, i) => (
                  <li key={i} className="flex items-start gap-2 text-[12px] text-amber-100/90">
                    <span className="mt-1.5 h-1.5 w-1.5 flex-none rounded-full bg-amber-400" />
                    <span>{r}</span>
                  </li>
                ))}
              </ul>
            </div>
          ) : null}

          {report.dbInconsistencies > 0 || report.backupIncomplete > 0 || report.archivedNotVerified > 0 ? (
            <div className="mt-3 flex flex-wrap gap-4 border-t border-white/10 pt-3 text-[11px] text-zinc-400">
              {report.archivedNotVerified > 0 ? (
                <span>{formatNumber(report.archivedNotVerified)} archived but not verified</span>
              ) : null}
              {report.backupIncomplete > 0 ? (
                <span>{formatNumber(report.backupIncomplete)} with incomplete backups</span>
              ) : null}
              {report.dbInconsistencies > 0 ? (
                <span>{formatNumber(report.dbInconsistencies)} database inconsistencies</span>
              ) : null}
            </div>
          ) : null}
        </div>
      </div>
    </div>
  );
}

function ClassFileList({ stat }: { stat: ClassStatDTO }) {
  const [open, setOpen] = useState(false);
  const meta = metaFor(stat.class);
  const shown = (stat.files ?? []).slice(0, MAX_ROWS);
  const hidden = stat.count - shown.length;
  return (
    <div className="overflow-hidden rounded-lg border border-zinc-800 bg-zinc-900/60">
      <button
        onClick={() => setOpen((o) => !o)}
        className="flex w-full items-center gap-2 px-4 py-2.5 text-left transition hover:bg-zinc-800/40"
      >
        {open ? (
          <ChevronDownIcon className="h-4 w-4 text-zinc-500" />
        ) : (
          <ChevronRightIcon className="h-4 w-4 text-zinc-500" />
        )}
        <span className={`text-[13px] font-medium ${meta.accent}`}>{meta.label}</span>
        <span className="text-[11px] text-zinc-500">
          {formatNumber(stat.count)} files · {formatBytes(stat.bytes)}
        </span>
      </button>
      {open ? (
        <div className="border-t border-zinc-800">
          {shown.length === 0 ? (
            <p className="px-4 py-3 text-[12px] text-zinc-500">No file paths available.</p>
          ) : (
            <ul className="max-h-96 divide-y divide-zinc-800/60 overflow-y-auto">
              {shown.map((f, i) => (
                <li key={i} className="flex items-baseline gap-3 px-4 py-1.5">
                  <span className="truncate text-[12px] text-zinc-300" title={f}>
                    {baseName(f)}
                  </span>
                  <span className="selectable truncate font-mono text-[10px] text-zinc-600" title={f}>
                    {f}
                  </span>
                </li>
              ))}
            </ul>
          )}
          {hidden > 0 ? (
            <p className="border-t border-zinc-800 px-4 py-2 text-[11px] text-zinc-500">
              Showing {formatNumber(shown.length)} of {formatNumber(stat.count)} — {formatNumber(hidden)} more not listed.
            </p>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
