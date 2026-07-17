import { useEffect, useMemo, useRef, useState } from "react";
import { ArrowPathIcon, ClockIcon, ExclamationTriangleIcon } from "@heroicons/react/24/outline";
import {
  Button,
  Card,
  DataTable,
  PageHeader,
  Spinner,
  StatusBadge,
  type DataTableColumn,
} from "../components";
import {
  HistoryService,
  WailsEvents,
  type LogEntryDTO,
  type SessionDetail,
  type SessionDTO,
} from "../lib/api";
import { useAsyncData, useWailsEvent } from "../lib/hooks";
import { useToast } from "../lib/toast";
import { baseName, formatDate, formatDuration, formatNumber, formatRelative } from "../lib/format";

const PAGE_SIZE = 20;

/** Import History — sessions with a lazily-loaded per-session event drill-down. */
export function HistoryPage() {
  const toast = useToast();
  const [page, setPage] = useState(1);
  const sessions = useAsyncData(() => HistoryService.ListSessions(page, PAGE_SIZE));

  // Cache of session-id -> loaded detail so re-expanding a row is instant.
  const detailCache = useRef(new Map<string, SessionDetail>());

  useEffect(() => {
    void sessions.run().catch((e) => toast.fromError(e, "Failed to load import history"));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [page]);

  // A completed import may add or update a session row.
  useWailsEvent(WailsEvents.ImportCompleted, () => {
    detailCache.current.clear();
    void sessions.run({ silent: true });
  });

  const columns = useMemo<DataTableColumn<SessionDTO>[]>(
    () => [
      {
        id: "id",
        header: "Import ID",
        accessorKey: "id",
        cell: ({ row }) => (
          <span className="selectable font-mono text-[12px] text-zinc-400" title={row.original.id}>
            {row.original.id.slice(0, 8)}
          </span>
        ),
      },
      {
        id: "date",
        header: "Date",
        accessorFn: (s) => s.startedAt,
        cell: ({ row }) => (
          <span className="whitespace-nowrap text-zinc-300" title={formatDate(row.original.startedAt)}>
            {formatDate(row.original.startedAt)}
          </span>
        ),
      },
      {
        id: "source",
        header: "Source",
        accessorFn: (s) => s.sourceId,
        cell: ({ row }) =>
          row.original.sourceId ? (
            <span className="font-mono text-[11px] text-zinc-500" title={row.original.sourceId}>
              {row.original.sourceId.slice(0, 8)}
            </span>
          ) : (
            <span className="text-zinc-600">—</span>
          ),
      },
      {
        id: "duration",
        header: "Duration",
        accessorKey: "durationSeconds",
        cell: ({ row }) => (
          <span className="tabular-nums text-zinc-400">{formatDuration(row.original.durationSeconds)}</span>
        ),
      },
      {
        id: "files",
        header: "Files",
        accessorFn: (s) => s.filesImported,
        cell: ({ row }) => (
          <span className="tabular-nums text-zinc-300">
            {formatNumber(row.original.filesImported)}
            <span className="text-zinc-600"> / {formatNumber(row.original.filesScanned)}</span>
          </span>
        ),
      },
      {
        id: "duplicates",
        header: "Duplicates",
        accessorKey: "duplicates",
        cell: ({ row }) => (
          <span className={`tabular-nums ${row.original.duplicates > 0 ? "text-amber-400" : "text-zinc-500"}`}>
            {formatNumber(row.original.duplicates)}
          </span>
        ),
      },
      {
        id: "failures",
        header: "Failures",
        accessorKey: "failures",
        cell: ({ row }) => (
          <span className={`tabular-nums ${row.original.failures > 0 ? "text-red-400" : "text-zinc-500"}`}>
            {formatNumber(row.original.failures)}
          </span>
        ),
      },
      {
        id: "status",
        header: "Status",
        accessorKey: "status",
        cell: ({ row }) => (
          <div className="flex items-center gap-1.5">
            <StatusBadge status={row.original.status} />
            {isAdopt(row.original) ? <StatusBadge status="adopt" tone="neutral" label="Adopt" /> : null}
          </div>
        ),
      },
      {
        id: "destination",
        header: "Destination",
        accessorFn: (s) => s.destinationRoot,
        cell: ({ row }) =>
          row.original.destinationRoot ? (
            <span className="text-zinc-400" title={row.original.destinationRoot}>
              {baseName(row.original.destinationRoot) || row.original.destinationRoot}
            </span>
          ) : (
            <span className="text-zinc-600">—</span>
          ),
      },
    ],
    [],
  );

  const total = sessions.data?.total ?? 0;

  return (
    <div>
      <PageHeader
        title="Import History"
        description="Every import session with its outcome, counters, and per-file event log. Expand a row to see its chronological events."
        actions={
          <Button
            icon={ArrowPathIcon}
            onClick={() => {
              detailCache.current.clear();
              void sessions.run().catch((e) => toast.fromError(e, "Failed to load import history"));
            }}
            loading={sessions.loading && !!sessions.data}
          >
            Refresh
          </Button>
        }
      />

      <Card flush>
        <DataTable
          data={sessions.data?.items ?? []}
          columns={columns}
          loading={sessions.loading}
          getRowId={(s) => s.id}
          renderSubRow={(s) => <SessionDetailPanel session={s} cache={detailCache.current} />}
          pagination={{ page, pageSize: PAGE_SIZE, total, onPageChange: setPage }}
          emptyState={{
            icon: ClockIcon,
            title: "No imports yet",
            description: "Import sessions will appear here once you bring photos into your archive.",
          }}
        />
      </Card>
    </div>
  );
}

/** ImportSession.Notes / DTO mode can carry the adopt marker; read it defensively. */
function isAdopt(s: SessionDTO): boolean {
  const raw = (s as { mode?: unknown }).mode;
  if (typeof raw === "string") return raw.toLowerCase() === "adopt";
  return false;
}

function SessionDetailPanel({ session, cache }: { session: SessionDTO; cache: Map<string, SessionDetail> }) {
  const toast = useToast();
  const [detail, setDetail] = useState<SessionDetail | null>(() => cache.get(session.id) ?? null);
  const [loading, setLoading] = useState(!cache.has(session.id));
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (cache.has(session.id)) {
      setDetail(cache.get(session.id) ?? null);
      setLoading(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setError(null);
    HistoryService.SessionEvents(session.id)
      .then((d) => {
        if (cancelled) return;
        cache.set(session.id, d);
        setDetail(d);
      })
      .catch((e) => {
        if (cancelled) return;
        setError(toast.fromError(e, "Could not load session events"));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [session.id]);

  if (loading) {
    return (
      <div className="flex items-center gap-2 py-2 text-xs text-zinc-500">
        <Spinner size={14} /> Loading events…
      </div>
    );
  }
  if (error) {
    return <p className="py-2 text-xs text-red-400">{error}</p>;
  }

  const s = detail?.session ?? session;
  const events = detail?.events ?? [];

  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 gap-x-6 gap-y-2 text-[12px] sm:grid-cols-4">
        <DetailField label="Started" value={formatDate(s.startedAt)} />
        <DetailField label="Completed" value={formatDate(s.completedAt)} />
        <DetailField label="Duration" value={formatDuration(s.durationSeconds)} />
        <DetailField label="Mode" value={isAdopt(s) ? "Adopt in place" : "Copy"} />
        <DetailField label="Scanned" value={formatNumber(s.filesScanned)} />
        <DetailField label="Imported" value={formatNumber(s.filesImported)} />
        <DetailField label="Skipped" value={formatNumber(s.skipped)} />
        <DetailField label="Failures" value={formatNumber(s.failures)} />
        <div className="col-span-2 sm:col-span-4">
          <div className="text-[11px] text-zinc-600">Destination</div>
          <div className="selectable mt-0.5 font-mono text-[11px] text-zinc-400" title={s.destinationRoot}>
            {s.destinationRoot || "—"}
          </div>
        </div>
      </div>

      <div>
        <h4 className="mb-2 text-[11px] font-semibold tracking-wide text-zinc-500 uppercase">
          Events {events.length > 0 ? `(${formatNumber(events.length)})` : ""}
        </h4>
        {events.length === 0 ? (
          <p className="flex items-center gap-2 py-2 text-xs text-zinc-500">
            <ExclamationTriangleIcon className="h-4 w-4" /> No log events recorded for this session.
          </p>
        ) : (
          <ul className="space-y-1">
            {events.map((ev) => (
              <EventRow key={ev.id} event={ev} />
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}

function DetailField({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <div className="text-[11px] text-zinc-600">{label}</div>
      <div className="mt-0.5 text-zinc-300">{value}</div>
    </div>
  );
}

const LEVEL_DOT: Record<string, string> = {
  error: "bg-red-400",
  warn: "bg-amber-400",
  warning: "bg-amber-400",
  info: "bg-blue-400",
  debug: "bg-zinc-500",
};

function EventRow({ event }: { event: LogEntryDTO }) {
  const dot = LEVEL_DOT[event.level.toLowerCase()] ?? "bg-zinc-500";
  return (
    <li className="flex items-start gap-2.5 rounded-md px-2 py-1 hover:bg-zinc-800/30">
      <span className={`mt-1.5 h-1.5 w-1.5 flex-none rounded-full ${dot}`} title={event.level} />
      <span className="mt-0.5 flex-none rounded bg-zinc-800/70 px-1.5 py-0.5 font-mono text-[10px] text-zinc-400">
        {event.subsystem || "—"}
      </span>
      <span className="min-w-0 flex-1 text-[12px] text-zinc-300">{event.message}</span>
      <span className="flex-none text-[10px] whitespace-nowrap text-zinc-600" title={formatDate(event.timestamp)}>
        {formatRelative(event.timestamp)}
      </span>
    </li>
  );
}
