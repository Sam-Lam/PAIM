import { useEffect, useMemo, useRef, useState } from "react";
import {
  ArrowPathIcon,
  ArrowUturnLeftIcon,
  CloudArrowUpIcon,
  PauseIcon,
  PlayIcon,
  XMarkIcon,
} from "@heroicons/react/24/outline";
import {
  Button,
  Card,
  ConfirmDialog,
  DataTable,
  PageHeader,
  ProgressBar,
  Stat,
  StatusBadge,
  type DataTableColumn,
} from "../components";
import {
  BackupService,
  WailsEvents,
  type BackupJobDTO,
  type BackupProgress,
  type BackupQueueChanged,
  type QueueSummaryDTO,
} from "../lib/api";
import { useAsyncData, usePoll, useWailsEvent } from "../lib/hooks";
import { useToast } from "../lib/toast";
import { baseName, formatNumber } from "../lib/format";

const PAGE_SIZE = 25;

const TABS: { key: string; label: string }[] = [
  { key: "", label: "All" },
  { key: "pending", label: "Pending" },
  { key: "running", label: "Running" },
  { key: "failed", label: "Failed" },
  { key: "completed", label: "Completed" },
  { key: "paused", label: "Paused" },
  { key: "cancelled", label: "Cancelled" },
];

/** Backup Queue — the SQLite-persisted job queue with live progress and worker controls. */
export function BackupQueuePage() {
  const toast = useToast();
  const [filter, setFilter] = useState("");
  const [page, setPage] = useState(1);
  const [progress, setProgress] = useState<Record<string, BackupProgress>>({});
  const [cancelJob, setCancelJob] = useState<BackupJobDTO | null>(null);
  const [cancelling, setCancelling] = useState(false);
  const [busyAll, setBusyAll] = useState(false);

  const summary = useAsyncData(() => BackupService.QueueSummary());
  const jobs = useAsyncData(() => BackupService.ListJobs(filter, page, PAGE_SIZE));

  const refreshAll = (silent = true) => {
    void summary.run({ silent });
    void jobs.run({ silent });
  };

  // Load list when filter/page changes.
  useEffect(() => {
    void jobs.run().catch((e) => toast.fromError(e, "Failed to load backup jobs"));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [filter, page]);

  // Initial summary + 10s poll fallback.
  usePoll(() => {
    void summary.run({ silent: true });
    void jobs.run({ silent: true });
  }, 10000);

  // Live per-upload progress.
  useWailsEvent<BackupProgress>(WailsEvents.BackupProgress, (data) => {
    setProgress((p) => ({ ...p, [data.jobId]: data }));
  });

  // Debounced refresh on any queue change.
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  useWailsEvent<BackupQueueChanged>(WailsEvents.BackupQueueChanged, (data) => {
    if (data?.summary) summary.setData(data.summary);
    if (debounceRef.current) clearTimeout(debounceRef.current);
    debounceRef.current = setTimeout(() => refreshAll(true), 500);
  });
  useEffect(
    () => () => {
      if (debounceRef.current) clearTimeout(debounceRef.current);
    },
    [],
  );

  const jobAction = async (fn: () => Promise<void>, label: string) => {
    try {
      await fn();
      toast.success(label);
      refreshAll(true);
    } catch (e) {
      toast.fromError(e, "Action failed");
    }
  };

  const confirmCancel = async () => {
    if (!cancelJob) return;
    setCancelling(true);
    try {
      await BackupService.Cancel(cancelJob.id);
      toast.success("Job cancelled");
      setCancelJob(null);
      refreshAll(true);
    } catch (e) {
      toast.fromError(e, "Could not cancel job");
    } finally {
      setCancelling(false);
    }
  };

  const pauseAll = () =>
    void (async () => {
      setBusyAll(true);
      try {
        await BackupService.PauseAll();
        toast.success("Paused all pending jobs");
        refreshAll(true);
      } catch (e) {
        toast.fromError(e, "Could not pause all");
      } finally {
        setBusyAll(false);
      }
    })();

  const resumeAll = () =>
    void (async () => {
      setBusyAll(true);
      try {
        await BackupService.ResumeAll();
        toast.success("Resumed all paused jobs");
        refreshAll(true);
      } catch (e) {
        toast.fromError(e, "Could not resume all");
      } finally {
        setBusyAll(false);
      }
    })();

  const columns = useMemo<DataTableColumn<BackupJobDTO>[]>(
    () => [
      {
        id: "file",
        header: "File",
        accessorFn: (j) => j.filename,
        cell: ({ row }) => {
          const j = row.original;
          return (
            <div className="min-w-0">
              <div className="truncate font-medium text-zinc-200" title={j.archivePath}>
                {j.filename || baseName(j.archivePath) || j.assetId.slice(0, 8)}
              </div>
              {j.archivePath ? (
                <div className="selectable truncate font-mono text-[10px] text-zinc-600" title={j.archivePath}>
                  {j.archivePath}
                </div>
              ) : null}
            </div>
          );
        },
      },
      {
        id: "destination",
        header: "Destination",
        accessorFn: (j) => j.destination || j.plugin,
        cell: ({ row }) => {
          const j = row.original;
          return (
            <div className="min-w-0">
              <div className="text-zinc-300">{j.plugin || "—"}</div>
              {j.destination ? (
                <div className="truncate text-[10px] text-zinc-600" title={j.destination}>
                  {j.destination}
                </div>
              ) : null}
            </div>
          );
        },
      },
      {
        id: "progress",
        header: "Progress",
        enableSorting: false,
        cell: ({ row }) => {
          const j = row.original;
          if (j.status !== "running") return <span className="text-zinc-600">—</span>;
          const p = progress[j.id];
          const pct = p && p.bytesTotal > 0 ? (p.bytesDone / p.bytesTotal) * 100 : null;
          return <ProgressBar percent={pct} size="sm" showPercent className="w-40" />;
        },
      },
      {
        id: "retries",
        header: "Retries",
        accessorKey: "retries",
        cell: ({ row }) => (
          <span className={`tabular-nums ${row.original.retries > 0 ? "text-amber-400" : "text-zinc-500"}`}>
            {row.original.retries}
          </span>
        ),
      },
      {
        id: "status",
        header: "Status",
        accessorKey: "status",
        cell: ({ row }) => <StatusBadge status={row.original.status} />,
      },
      {
        id: "error",
        header: "Error",
        enableSorting: false,
        cell: ({ row }) =>
          row.original.errorMessage ? (
            <span className="block max-w-[16rem] truncate text-[12px] text-red-400/90" title={row.original.errorMessage}>
              {row.original.errorMessage}
            </span>
          ) : (
            <span className="text-zinc-600">—</span>
          ),
      },
      {
        id: "actions",
        header: "",
        enableSorting: false,
        cell: ({ row }) => {
          const j = row.original;
          return (
            <div className="flex items-center justify-end gap-1.5">
              {j.status === "failed" ? (
                <Button size="sm" variant="secondary" icon={ArrowPathIcon} onClick={() => void jobAction(() => BackupService.Retry(j.id), "Job requeued")}>
                  Retry
                </Button>
              ) : null}
              {j.status === "pending" ? (
                <Button size="sm" variant="ghost" icon={PauseIcon} onClick={() => void jobAction(() => BackupService.Pause(j.id), "Job paused")}>
                  Pause
                </Button>
              ) : null}
              {j.status === "paused" ? (
                <Button size="sm" variant="ghost" icon={PlayIcon} onClick={() => void jobAction(() => BackupService.Resume(j.id), "Job resumed")}>
                  Resume
                </Button>
              ) : null}
              {j.status === "pending" || j.status === "paused" || j.status === "failed" ? (
                <Button size="sm" variant="ghost" icon={XMarkIcon} onClick={() => setCancelJob(j)}>
                  Cancel
                </Button>
              ) : null}
            </div>
          );
        },
      },
    ],
    // Rebuild when live progress ticks so running bars update.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [progress],
  );

  const s = summary.data;
  const total = jobs.data?.total ?? 0;

  return (
    <div>
      <PageHeader
        title="Backup Queue"
        description="The SQLite-persisted backup queue. Jobs run automatically after import; pause, resume, retry, or cancel them here."
        actions={
          <>
            <Button icon={PauseIcon} variant="secondary" onClick={pauseAll} loading={busyAll}>
              Pause all
            </Button>
            <Button icon={ArrowUturnLeftIcon} variant="secondary" onClick={resumeAll} loading={busyAll}>
              Resume all
            </Button>
          </>
        }
      />

      <div className="mb-5 grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
        <Stat label="Pending" value={formatNumber(s?.pending ?? 0)} tone={s && s.pending > 0 ? "warn" : "default"} icon={CloudArrowUpIcon} />
        <Stat label="Running" value={formatNumber(s?.running ?? 0)} tone={s && s.running > 0 ? "accent" : "default"} />
        <Stat label="Completed" value={formatNumber(s?.completed ?? 0)} tone="success" />
        <Stat label="Failed" value={formatNumber(s?.failed ?? 0)} tone={s && s.failed > 0 ? "danger" : "default"} />
        <Stat label="Paused" value={formatNumber(s?.paused ?? 0)} />
        <Stat label="Cancelled" value={formatNumber(s?.cancelled ?? 0)} />
      </div>

      <Card flush>
        <div className="flex flex-wrap gap-1 border-b border-zinc-800 px-3 py-2">
          {TABS.map((t) => (
            <button
              key={t.key}
              onClick={() => {
                setFilter(t.key);
                setPage(1);
              }}
              className={`rounded-md px-2.5 py-1 text-[12px] font-medium transition ${
                filter === t.key ? "bg-zinc-800 text-zinc-100" : "text-zinc-500 hover:text-zinc-300"
              }`}
            >
              {t.label}
              <span className="ml-1.5 tabular-nums text-zinc-600">{tabCount(s, t.key)}</span>
            </button>
          ))}
        </div>

        <DataTable
          data={jobs.data?.items ?? []}
          columns={columns}
          loading={jobs.loading}
          getRowId={(j) => j.id}
          pagination={{ page, pageSize: PAGE_SIZE, total, onPageChange: setPage }}
          emptyState={{
            icon: CloudArrowUpIcon,
            title: filter ? `No ${filter} jobs` : "No backup jobs",
            description: "Backup jobs are created automatically when you import photos into your archive.",
          }}
        />
      </Card>

      <ConfirmDialog
        open={!!cancelJob}
        title="Cancel this backup job?"
        description={
          <span>
            Cancelling <span className="font-medium text-zinc-200">{cancelJob?.filename}</span> removes it from the
            active queue. Your archived original is never touched — you can re-enqueue a backup later.
          </span>
        }
        confirmLabel="Cancel job"
        cancelLabel="Keep job"
        loading={cancelling}
        onConfirm={() => void confirmCancel()}
        onCancel={() => (cancelling ? undefined : setCancelJob(null))}
      />
    </div>
  );
}

function tabCount(s: QueueSummaryDTO | null, key: string): string {
  if (!s) return "";
  const v =
    key === ""
      ? s.total
      : key === "pending"
        ? s.pending
        : key === "running"
          ? s.running
          : key === "failed"
            ? s.failed
            : key === "completed"
              ? s.completed
              : key === "paused"
                ? s.paused
                : key === "cancelled"
                  ? s.cancelled
                  : 0;
  return formatNumber(v);
}
