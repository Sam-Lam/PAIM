import { useEffect, useMemo, useState } from "react";
import {
  ArrowDownTrayIcon,
  DocumentTextIcon,
  MagnifyingGlassIcon,
  XMarkIcon,
} from "@heroicons/react/24/outline";
import {
  Button,
  Card,
  DataTable,
  PageHeader,
  StatusBadge,
  type BadgeTone,
  type DataTableColumn,
} from "../components";
import { LogService, WailsEvents, type LogEntryDTO, type LogExportProgress } from "../lib/api";
import { useAsyncData, usePoll, useWailsEvent } from "../lib/hooks";
import { useToast } from "../lib/toast";
import { formatDate, formatNumber } from "../lib/format";

const PAGE_SIZE = 50;
// Canonical uppercase — must match slog.Level.String() values stored in the DB.
const LEVELS = ["DEBUG", "INFO", "WARN", "ERROR"];

/** Convert a datetime-local value ("2026-07-17T15:30") to an ISO string, or "". */
function localToISO(local: string): string {
  if (!local) return "";
  const d = new Date(local);
  return isNaN(d.getTime()) ? "" : d.toISOString();
}

const LEVEL_TONE: Record<string, BadgeTone> = {
  error: "danger",
  warn: "warn",
  warning: "warn",
  info: "info",
  debug: "muted",
};

/** Logs — searchable, filterable structured log with JSON/CSV export and live tail. */
export function LogsPage() {
  const toast = useToast();
  const [queryInput, setQueryInput] = useState("");
  const [query, setQuery] = useState("");
  const [level, setLevel] = useState("");
  const [subsystem, setSubsystem] = useState("");
  const [fromLocal, setFromLocal] = useState("");
  const [toLocal, setToLocal] = useState("");
  const [page, setPage] = useState(1);
  const [exporting, setExporting] = useState(false);
  const [exportRows, setExportRows] = useState(0);

  useWailsEvent<LogExportProgress>(WailsEvents.LogExportProgress, (p) => {
    setExportRows(p.rowsWritten);
  });

  const fromISO = localToISO(fromLocal);
  const toISO = localToISO(toLocal);

  const search = useAsyncData(() =>
    LogService.Search(query, level, subsystem, fromISO, toISO, page, PAGE_SIZE),
  );
  const subsystems = useAsyncData(() => LogService.Subsystems());

  // Debounce the text query (300ms); applying it resets to page 1.
  useEffect(() => {
    const id = setTimeout(() => {
      setQuery((prev) => {
        if (prev !== queryInput) setPage(1);
        return queryInput;
      });
    }, 300);
    return () => clearTimeout(id);
  }, [queryInput]);

  useEffect(() => {
    void subsystems.run().catch(() => undefined);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    void search.run().catch((e) => toast.fromError(e, "Failed to search logs"));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [query, level, subsystem, fromISO, toISO, page]);

  const filtersActive = !!query || !!level || !!subsystem || !!fromISO || !!toISO;

  // Live tail: silently refresh every 5s, but ONLY on page 1 with no active
  // filters — so a user reading filtered/older results is never disrupted.
  usePoll(() => {
    if (page === 1 && !filtersActive) void search.run({ silent: true });
  }, 5000);

  const doExport = async (format: "json" | "csv") => {
    setExporting(true);
    setExportRows(0);
    try {
      const path = await LogService.Export(query, level, subsystem, fromISO, toISO, format);
      if (path) toast.success("Log exported", path);
      else toast.info("Export cancelled");
    } catch (e) {
      toast.fromError(e, "Export failed");
    } finally {
      setExporting(false);
    }
  };

  const clearFilters = () => {
    setQueryInput("");
    setQuery("");
    setLevel("");
    setSubsystem("");
    setFromLocal("");
    setToLocal("");
    setPage(1);
  };

  const columns = useMemo<DataTableColumn<LogEntryDTO>[]>(
    () => [
      {
        id: "timestamp",
        header: "Timestamp",
        accessorKey: "timestamp",
        enableSorting: false,
        cell: ({ row }) => (
          <span className="whitespace-nowrap text-zinc-400" title={row.original.timestamp}>
            {formatDate(row.original.timestamp)}
          </span>
        ),
      },
      {
        id: "level",
        header: "Level",
        accessorKey: "level",
        enableSorting: false,
        cell: ({ row }) => (
          <StatusBadge
            status={row.original.level}
            tone={LEVEL_TONE[row.original.level.toLowerCase()] ?? "neutral"}
            label={row.original.level.toUpperCase()}
            dot
          />
        ),
      },
      {
        id: "subsystem",
        header: "Subsystem",
        accessorKey: "subsystem",
        enableSorting: false,
        cell: ({ row }) => (
          <span className="rounded bg-zinc-800/70 px-1.5 py-0.5 font-mono text-[11px] text-zinc-400">
            {row.original.subsystem || "—"}
          </span>
        ),
      },
      {
        id: "message",
        header: "Message",
        accessorKey: "message",
        enableSorting: false,
        cell: ({ row }) => <span className="text-zinc-300">{row.original.message}</span>,
      },
    ],
    [],
  );

  const total = search.data?.total ?? 0;

  return (
    <div>
      <PageHeader
        title="Logs"
        description="Search, filter, and export the structured application log. Expand a row to see its metadata."
        actions={
          <>
            {exporting ? (
              <span className="flex items-center text-[11px] text-zinc-500 tabular-nums">
                Exporting… {exportRows > 0 ? `${formatNumber(exportRows)} rows` : ""}
              </span>
            ) : null}
            <Button icon={ArrowDownTrayIcon} variant="secondary" onClick={() => void doExport("json")} loading={exporting} disabled={exporting}>
              Export JSON
            </Button>
            <Button icon={ArrowDownTrayIcon} variant="secondary" onClick={() => void doExport("csv")} loading={exporting} disabled={exporting}>
              Export CSV
            </Button>
          </>
        }
      />

      <Card className="mb-5">
        <div className="flex flex-wrap items-end gap-3">
          <label className="min-w-[16rem] flex-1">
            <span className="mb-1 block text-[11px] font-medium text-zinc-500">Search</span>
            <div className="relative">
              <MagnifyingGlassIcon className="pointer-events-none absolute top-1/2 left-2.5 h-4 w-4 -translate-y-1/2 text-zinc-600" />
              <input
                value={queryInput}
                onChange={(e) => setQueryInput(e.target.value)}
                placeholder="Search messages…"
                className="w-full rounded-md border border-zinc-700 bg-zinc-950 py-1.5 pr-3 pl-8 text-[13px] text-zinc-200 outline-none focus:border-blue-500"
              />
            </div>
          </label>

          <label>
            <span className="mb-1 block text-[11px] font-medium text-zinc-500">Level</span>
            <select
              value={level}
              onChange={(e) => {
                setLevel(e.target.value);
                setPage(1);
              }}
              className="rounded-md border border-zinc-700 bg-zinc-950 px-2.5 py-1.5 text-[13px] text-zinc-200 outline-none focus:border-blue-500"
            >
              <option value="">All levels</option>
              {LEVELS.map((l) => (
                <option key={l} value={l}>
                  {l.toUpperCase()}
                </option>
              ))}
            </select>
          </label>

          <label>
            <span className="mb-1 block text-[11px] font-medium text-zinc-500">Subsystem</span>
            <select
              value={subsystem}
              onChange={(e) => {
                setSubsystem(e.target.value);
                setPage(1);
              }}
              className="rounded-md border border-zinc-700 bg-zinc-950 px-2.5 py-1.5 text-[13px] text-zinc-200 outline-none focus:border-blue-500"
            >
              <option value="">All subsystems</option>
              {(subsystems.data ?? []).map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </select>
          </label>

          <label>
            <span className="mb-1 block text-[11px] font-medium text-zinc-500">From</span>
            <input
              type="datetime-local"
              value={fromLocal}
              onChange={(e) => {
                setFromLocal(e.target.value);
                setPage(1);
              }}
              className="rounded-md border border-zinc-700 bg-zinc-950 px-2.5 py-1.5 text-[13px] text-zinc-200 outline-none focus:border-blue-500"
            />
          </label>

          <label>
            <span className="mb-1 block text-[11px] font-medium text-zinc-500">To</span>
            <input
              type="datetime-local"
              value={toLocal}
              onChange={(e) => {
                setToLocal(e.target.value);
                setPage(1);
              }}
              className="rounded-md border border-zinc-700 bg-zinc-950 px-2.5 py-1.5 text-[13px] text-zinc-200 outline-none focus:border-blue-500"
            />
          </label>

          {filtersActive ? (
            <Button icon={XMarkIcon} variant="ghost" onClick={clearFilters}>
              Clear
            </Button>
          ) : null}
        </div>
        {!filtersActive && page === 1 ? (
          <p className="mt-2 text-[11px] text-zinc-600">Live tail on — newest entries refresh automatically.</p>
        ) : null}
      </Card>

      <Card flush>
        <DataTable
          data={search.data?.items ?? []}
          columns={columns}
          loading={search.loading}
          getRowId={(l) => String(l.id)}
          dense
          renderSubRow={(l) => <MetadataPanel entry={l} />}
          pagination={{ page, pageSize: PAGE_SIZE, total, onPageChange: setPage }}
          emptyState={{
            icon: DocumentTextIcon,
            title: filtersActive ? "No matching log entries" : "No log entries",
            description: filtersActive
              ? "Try widening your search or clearing the filters."
              : "Application activity will appear here as you use PAIM.",
          }}
        />
      </Card>
    </div>
  );
}

function MetadataPanel({ entry }: { entry: LogEntryDTO }) {
  const pretty = useMemo(() => prettyJSON(entry.metadataJson), [entry.metadataJson]);
  if (!pretty) {
    return <p className="py-1 text-[12px] text-zinc-500">No metadata recorded for this entry.</p>;
  }
  return (
    <pre className="selectable overflow-x-auto rounded-md border border-zinc-800 bg-zinc-950/60 p-3 font-mono text-[11px] leading-relaxed text-zinc-300">
      {pretty}
    </pre>
  );
}

function prettyJSON(raw: string): string {
  if (!raw || raw === "{}" || raw === "null") return "";
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}
