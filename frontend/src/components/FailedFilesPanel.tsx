import { useCallback, useEffect, useState } from "react";
import { ArrowPathIcon, XMarkIcon } from "@heroicons/react/24/outline";
import { Button } from "./Button";
import { ConfirmDialog } from "./ConfirmDialog";
import { StatusBadge, type BadgeTone } from "./StatusBadge";
import {
  HistoryService,
  ImportService,
  type ImportFailureDTO,
  type PageResult,
} from "../lib/api";
import { useToast } from "../lib/toast";

const PAGE_SIZE = 25;

// statusBadge maps a failure status to a tone + label for the pill.
function statusBadge(status: string): { tone: BadgeTone; label: string } {
  switch (status) {
    case "retried":
      return { tone: "success", label: "Retried" };
    case "dismissed":
      return { tone: "muted", label: "Dismissed" };
    default:
      return { tone: "warn", label: "Open" };
  }
}

/**
 * FailedFilesPanel lists the structured per-file import failures for one session
 * (path, stage, error, status) with per-file Retry and Dismiss actions. It is
 * shared by the Import completion panel and the Import History session detail.
 *
 * Sessions imported before structured records existed have a Failures counter
 * but no rows; when `sessionFailures > 0` yet the server returns zero records the
 * panel shows the legacy note instead of a list. Bindings arrays are null-guarded
 * (`?? []`). `onChanged` lets the parent refresh its own counters after a
 * resolution.
 */
export function FailedFilesPanel({
  sessionId,
  sessionFailures,
  onChanged,
}: {
  sessionId: string;
  sessionFailures: number;
  onChanged?: () => void;
}) {
  const toast = useToast();
  const [page, setPage] = useState<PageResult<ImportFailureDTO> | null>(null);
  const [loading, setLoading] = useState(true);
  const [pageNum, setPageNum] = useState(1);
  const [busyId, setBusyId] = useState("");
  const [dismissTarget, setDismissTarget] = useState<ImportFailureDTO | null>(null);
  const [dismissing, setDismissing] = useState(false);
  // Per-file inline retry outcome messages, keyed by failure ID.
  const [retryMsg, setRetryMsg] = useState<Record<string, string>>({});

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const res = await HistoryService.ListSessionFailures(sessionId, pageNum, PAGE_SIZE);
      setPage(res);
    } catch (err) {
      toast.fromError(err, "Could not load failed files");
    } finally {
      setLoading(false);
    }
  }, [sessionId, pageNum, toast]);

  useEffect(() => {
    void load();
  }, [load]);

  const items = page?.items ?? [];
  const total = page?.total ?? 0;

  const retry = useCallback(
    async (f: ImportFailureDTO) => {
      setBusyId(f.id);
      setRetryMsg((m) => ({ ...m, [f.id]: "" }));
      try {
        const res = await ImportService.RetryFailedFile(f.id);
        if (res.success) {
          toast.success("File re-imported", f.path);
          onChanged?.();
          await load();
        } else {
          setRetryMsg((m) => ({
            ...m,
            [f.id]: `Retry failed at ${res.op || "import"}: ${res.error}`,
          }));
        }
      } catch (err) {
        // The most common case: the source file no longer exists (dismiss instead).
        setRetryMsg((m) => ({ ...m, [f.id]: toast.fromError(err, "Retry failed") }));
      } finally {
        setBusyId("");
      }
    },
    [load, onChanged, toast],
  );

  const confirmDismiss = useCallback(
    async (reason?: string) => {
      if (!dismissTarget) return;
      setDismissing(true);
      try {
        await HistoryService.DismissFailure(dismissTarget.id, reason ?? "");
        toast.info("Failure dismissed", dismissTarget.path);
        onChanged?.();
        setDismissTarget(null);
        await load();
      } catch (err) {
        toast.fromError(err, "Could not dismiss");
      } finally {
        setDismissing(false);
      }
    },
    [dismissTarget, load, onChanged, toast],
  );

  // Nothing failed at all: render nothing.
  if (sessionFailures <= 0 && total === 0) return null;

  // Legacy session: a Failures counter but no structured rows.
  if (!loading && total === 0 && sessionFailures > 0) {
    return (
      <div className="rounded-lg border border-amber-800/40 bg-amber-950/20 p-3">
        <h4 className="text-xs font-semibold text-zinc-200">
          {sessionFailures} failed {sessionFailures === 1 ? "file" : "files"}
        </h4>
        <p className="mt-1 text-[12px] leading-relaxed text-zinc-400">
          This session was imported before per-file failure records existed, so only the
          count is available. See the session's log events below for details. Structured
          Retry/Dismiss records exist for imports run from this version onward.
        </p>
      </div>
    );
  }

  const pageCount = Math.max(1, Math.ceil(total / PAGE_SIZE));

  return (
    <div className="rounded-lg border border-zinc-800 bg-zinc-900/40 p-3">
      <div className="mb-2 flex items-center justify-between">
        <h4 className="text-xs font-semibold text-zinc-200">
          Failed files{total > 0 ? ` (${total})` : ""}
        </h4>
        {pageCount > 1 ? (
          <div className="flex items-center gap-2 text-[11px] text-zinc-500">
            <button
              className="rounded px-1.5 py-0.5 hover:bg-zinc-800 disabled:opacity-40"
              onClick={() => setPageNum((p) => Math.max(1, p - 1))}
              disabled={pageNum <= 1 || loading}
            >
              Prev
            </button>
            <span className="tabular-nums">
              {pageNum} / {pageCount}
            </span>
            <button
              className="rounded px-1.5 py-0.5 hover:bg-zinc-800 disabled:opacity-40"
              onClick={() => setPageNum((p) => Math.min(pageCount, p + 1))}
              disabled={pageNum >= pageCount || loading}
            >
              Next
            </button>
          </div>
        ) : null}
      </div>

      {loading && items.length === 0 ? (
        <p className="text-[12px] text-zinc-500">Loading…</p>
      ) : (
        <ul className="space-y-2">
          {items.map((f) => {
            const badge = statusBadge(f.status);
            const open = f.status === "open";
            const msg = retryMsg[f.id];
            return (
              <li key={f.id} className="rounded-md border border-zinc-800 bg-zinc-950/40 p-2.5">
                <div className="flex items-start justify-between gap-2">
                  <div className="min-w-0 flex-1">
                    <p className="selectable break-all font-mono text-[11px] text-zinc-300">
                      {f.path}
                    </p>
                    <div className="mt-1 flex flex-wrap items-center gap-2">
                      <StatusBadge status={f.status} tone={badge.tone} label={badge.label} />
                      <StatusBadge status={f.op} tone="neutral" label={`stage: ${f.op}`} />
                    </div>
                    <p className="mt-1 break-words text-[11px] leading-relaxed text-zinc-500">
                      {f.errorMessage}
                    </p>
                    {f.status === "dismissed" && f.dismissReason ? (
                      <p className="mt-1 text-[11px] italic text-zinc-500">
                        Dismissed: {f.dismissReason}
                      </p>
                    ) : null}
                    {msg ? <p className="mt-1 text-[11px] text-amber-400">{msg}</p> : null}
                  </div>
                  {open ? (
                    <div className="flex flex-none items-center gap-1.5">
                      <Button
                        size="sm"
                        variant="secondary"
                        icon={ArrowPathIcon}
                        loading={busyId === f.id}
                        onClick={() => void retry(f)}
                      >
                        Retry
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        icon={XMarkIcon}
                        disabled={busyId === f.id}
                        onClick={() => setDismissTarget(f)}
                      >
                        Dismiss
                      </Button>
                    </div>
                  ) : null}
                </div>
              </li>
            );
          })}
        </ul>
      )}

      <ConfirmDialog
        open={dismissTarget != null}
        variant="primary"
        title="Dismiss this failure?"
        confirmLabel="Dismiss"
        description={
          dismissTarget ? (
            <span>
              Marks the failed file resolved without importing it — use this when the file no
              longer exists or you don't need it. The record is kept (never deleted).
              <span className="mt-1 block break-all font-mono text-[11px] text-zinc-500">
                {dismissTarget.path}
              </span>
            </span>
          ) : null
        }
        reasonLabel="Reason (optional)"
        reasonPlaceholder="e.g. deleted off the card"
        loading={dismissing}
        onConfirm={(reason) => void confirmDismiss(reason)}
        onCancel={() => setDismissTarget(null)}
      />
    </div>
  );
}
