import { useMemo } from "react";
import { Link } from "@tanstack/react-router";
import {
  ArrowPathIcon,
  ArrowTrendingUpIcon,
  CircleStackIcon,
  CloudArrowUpIcon,
  DocumentDuplicateIcon,
  ExclamationTriangleIcon,
  FilmIcon,
  InboxArrowDownIcon,
  PhotoIcon,
  Squares2X2Icon,
} from "@heroicons/react/24/outline";
import {
  Button,
  Card,
  EmptyState,
  LoadingBlock,
  PageHeader,
  SafeToEraseBadge,
  Stat,
  StatusBadge,
} from "../components";
import { DashboardService, WailsEvents, type MonthCountDTO, type SourceDTO } from "../lib/api";
import { useAsyncData, usePoll, useWailsEvent } from "../lib/hooks";
import { useToast } from "../lib/toast";
import {
  formatBytes,
  formatMonthLabel,
  formatMonthLong,
  formatNumber,
  formatRelative,
} from "../lib/format";

export function DashboardPage() {
  const toast = useToast();
  const { data: stats, loading, error, run } = useAsyncData(() => DashboardService.GetStats());

  // Poll every 30s (also fires the initial load), and refresh on relevant events.
  usePoll(() => {
    void run({ silent: true }).catch((e) => toast.fromError(e, "Failed to load dashboard"));
  }, 30_000);
  useWailsEvent(WailsEvents.ImportCompleted, () => void run({ silent: true }));
  useWailsEvent(WailsEvents.BackupQueueChanged, () => void run({ silent: true }));

  const refresh = () => void run().catch((e) => toast.fromError(e, "Failed to load dashboard"));

  return (
    <div>
      <PageHeader
        title="Dashboard"
        description="Library health at a glance — assets, storage, pending work, and recent activity."
        actions={
          <Button icon={ArrowPathIcon} onClick={refresh} loading={loading && !!stats}>
            Refresh
          </Button>
        }
      />

      {loading && !stats ? (
        <LoadingBlock label="Loading dashboard…" />
      ) : error && !stats ? (
        <EmptyState
          icon={ExclamationTriangleIcon}
          title="Could not load the dashboard"
          description={error}
          action={<Button onClick={refresh}>Try again</Button>}
        />
      ) : stats ? (
        <div className="space-y-6">
          {/* Stat grid */}
          <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
            <Stat label="Total Assets" value={formatNumber(stats.totals.assets)} icon={Squares2X2Icon} />
            <Stat label="Photos" value={formatNumber(stats.totals.photos)} icon={PhotoIcon} />
            <Stat label="Videos" value={formatNumber(stats.totals.videos)} icon={FilmIcon} />
            <Stat
              label="Storage Used"
              value={formatBytes(stats.totals.storageBytes)}
              icon={CircleStackIcon}
            />
            <Stat
              label="Pending Imports"
              value={formatNumber(stats.pendingImports)}
              icon={InboxArrowDownIcon}
              tone={stats.pendingImports > 0 ? "warn" : "default"}
            />
            <Stat
              label="Pending Backups"
              value={formatNumber(stats.backupQueue.pending)}
              icon={CloudArrowUpIcon}
              tone={stats.backupQueue.pending > 0 ? "accent" : "default"}
            />
            <Stat
              label="Failed Backups"
              value={formatNumber(stats.backupQueue.failed)}
              icon={ExclamationTriangleIcon}
              tone={stats.backupQueue.failed > 0 ? "danger" : "default"}
            />
            <Stat
              label="Duplicates"
              value={formatNumber(stats.duplicateCount)}
              icon={DocumentDuplicateIcon}
              tone={stats.duplicateCount > 0 ? "warn" : "default"}
            />
          </div>

          {/* Library growth */}
          <Card title="Library Growth" subtitle="Assets added per month (last 12 months)">
            <LibraryGrowthChart data={stats.libraryGrowth} />
          </Card>

          {/* Sources row */}
          <div className="grid gap-3 lg:grid-cols-2">
            <Card
              title="Recently Connected Sources"
              actions={
                <Link to="/sources" className="text-xs font-medium text-blue-400 hover:text-blue-300">
                  View all
                </Link>
              }
            >
              <SourceList sources={stats.recentSources} empty="No sources connected yet." />
            </Card>

            <Card title="Safe To Erase" subtitle="Sources fully archived and backed up">
              <SourceList sources={stats.safeToEraseSources} empty="No sources are safe to erase yet." showSafe />
            </Card>
          </div>

          {/* Recent activity */}
          <Card
            title="Recent Activity"
            actions={
              <Link to="/logs" className="text-xs font-medium text-blue-400 hover:text-blue-300">
                Open logs
              </Link>
            }
            flush
          >
            {stats.recentActivity.length === 0 ? (
              <EmptyState title="No recent activity" description="Import or backup activity will show up here." />
            ) : (
              <ul className="divide-y divide-zinc-800/60">
                {stats.recentActivity.map((entry) => (
                  <li key={entry.id} className="flex items-start gap-3 px-4 py-2.5">
                    <span className={`mt-1.5 h-2 w-2 flex-none rounded-full ${levelDot(entry.level)}`} />
                    <div className="min-w-0 flex-1">
                      <div className="flex items-center gap-2">
                        <span className="rounded bg-zinc-800 px-1.5 py-0.5 text-[10px] font-medium tracking-wide text-zinc-400 uppercase">
                          {entry.subsystem || "system"}
                        </span>
                        <span className="text-[11px] text-zinc-500">{formatRelative(entry.timestamp)}</span>
                      </div>
                      <p className="mt-0.5 truncate text-[13px] text-zinc-300" title={entry.message}>
                        {entry.message}
                      </p>
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </Card>
        </div>
      ) : null}
    </div>
  );
}

function levelDot(level: string): string {
  switch ((level || "").toLowerCase()) {
    case "error":
      return "bg-red-500";
    case "warn":
    case "warning":
      return "bg-amber-500";
    case "debug":
      return "bg-zinc-600";
    default:
      return "bg-blue-500";
  }
}

function SourceList({
  sources,
  empty,
  showSafe = false,
}: {
  sources: SourceDTO[];
  empty: string;
  showSafe?: boolean;
}) {
  if (!sources || sources.length === 0) {
    return <EmptyState title="Nothing here yet" description={empty} />;
  }
  return (
    <ul className="space-y-2">
      {sources.map((s) => (
        <li key={s.id} className="flex items-center justify-between gap-3 rounded-md border border-zinc-800 px-3 py-2">
          <div className="min-w-0">
            <div className="truncate text-[13px] font-medium text-zinc-200">
              {s.volumeLabel || s.model || s.hardwareSerial || "Unknown source"}
            </div>
            <div className="mt-0.5 flex items-center gap-2 text-[11px] text-zinc-500">
              <StatusBadge status={s.sourceType} tone="muted" />
              <span>Last seen {formatRelative(s.lastSeenAt)}</span>
            </div>
            {showSafe && s.safeToEraseReason ? (
              <p className="mt-1 truncate text-[11px] text-emerald-400/80" title={s.safeToEraseReason}>
                {s.safeToEraseReason}
              </p>
            ) : null}
          </div>
          {showSafe ? (
            <SafeToEraseBadge safe={s.safeToErase} />
          ) : (
            <span className="flex-none text-[11px] text-zinc-500">{formatNumber(s.importCount)} imports</span>
          )}
        </li>
      ))}
    </ul>
  );
}

/** Minimal inline-SVG bar chart — muted single-color bars, hover title labels. */
function LibraryGrowthChart({ data }: { data: MonthCountDTO[] }) {
  const months = useMemo(() => (data ?? []).slice(-12), [data]);
  const max = useMemo(() => Math.max(1, ...months.map((m) => m.count)), [months]);
  const total = useMemo(() => months.reduce((a, m) => a + m.count, 0), [months]);

  if (months.length === 0 || total === 0) {
    return (
      <EmptyState
        icon={ArrowTrendingUpIcon}
        title="No growth data yet"
        description="Once you import assets, monthly totals will chart here."
      />
    );
  }

  return (
    <div>
      <div className="flex items-end gap-1.5" style={{ height: 140 }}>
        {months.map((m) => {
          const h = Math.max(2, Math.round((m.count / max) * 120));
          return (
            <div key={m.month} className="flex flex-1 flex-col items-center justify-end gap-1.5">
              <div
                className="w-full rounded-t-sm bg-blue-500/70 transition-colors hover:bg-blue-400"
                style={{ height: h }}
                title={`${formatMonthLong(m.month)}: ${formatNumber(m.count)} assets`}
              />
              <span className="text-[10px] text-zinc-600">{formatMonthLabel(m.month)}</span>
            </div>
          );
        })}
      </div>
      <div className="mt-3 text-xs text-zinc-500">
        {formatNumber(total)} assets added in the last {months.length} months
      </div>
    </div>
  );
}
