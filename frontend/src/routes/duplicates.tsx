import { useCallback, useEffect, useMemo, useState } from "react";
import {
  ArrowPathIcon,
  ArrowRightCircleIcon,
  CheckIcon,
  ClipboardDocumentIcon,
  DocumentDuplicateIcon,
  ExclamationTriangleIcon,
  FolderArrowDownIcon,
  FolderOpenIcon,
  NoSymbolIcon,
  PhotoIcon,
  Square2StackIcon,
  TrashIcon,
  XMarkIcon,
} from "@heroicons/react/24/outline";
import {
  Button,
  Card,
  ConfirmDialog,
  EmptyState,
  LoadingBlock,
  PageHeader,
  ProgressBar,
  StatusBadge,
} from "../components";
import {
  BrowserService,
  CleanupService,
  DuplicateService,
  WailsEvents,
  type ActiveBulkResolveDTO,
  type AssetDTO,
  type BulkResolveProgress,
  type BulkResolveSummaryDTO,
  type DuplicateGroupDTO,
  type DuplicatePairDTO,
} from "../lib/api";
import { useAsyncData, useWailsEvent } from "../lib/hooks";
import { useToast } from "../lib/toast";
import { formatBytes, formatDate, formatNumber } from "../lib/format";

const PAGE_SIZE = 20;

type DupAction = "delete" | "move" | "ignore" | "keep_both";
type GroupBy = "none" | "folder" | "session";

interface Filter {
  groupBy: GroupBy;
  groupKey: string;
  sortBySize: boolean;
}

interface BatchPending {
  action: DupAction;
  destFolder: string;
  ids: string[];
  bytes: number;
  title: string;
  confirmLabel: string;
  variant: "danger" | "primary";
  requireWord?: string;
  description: React.ReactNode;
}

/** Duplicate Manager — bulk triage at scale: filter/group, multi-select, resolve as a background job. */
export function DuplicatesPage() {
  const toast = useToast();

  const [filter, setFilter] = useState<Filter>({ groupBy: "none", groupKey: "", sortBySize: false });
  const [page, setPage] = useState(1);

  // Selection: a set of concrete duplicate IDs. allInFilter marks that the set is
  // exactly the whole filtered set (so we can show the authoritative wasted bytes
  // from the filter totals rather than only the sizes of pages we have rendered).
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [allInFilter, setAllInFilter] = useState(false);
  // Sizes of every duplicate we have rendered, so an explicit selection can total
  // its wasted bytes without paging the whole set.
  const [sizeById, setSizeById] = useState<Map<string, number>>(new Map());

  const [pending, setPending] = useState<BatchPending | null>(null);
  const [starting, setStarting] = useState(false);

  // Background bulk-resolve state (re-attached on mount, driven by events).
  const [progress, setProgress] = useState<BulkResolveProgress | null>(null);
  const [summary, setSummary] = useState<BulkResolveSummaryDTO | null>(null);
  const [running, setRunning] = useState(false);

  const filterDTO = useMemo(
    () => ({
      groupBy: filter.groupBy === "none" ? "" : filter.groupBy,
      groupKey: filter.groupKey,
      sortBySize: filter.sortBySize,
    }),
    [filter],
  );

  const stats = useAsyncData(() => DuplicateService.DuplicateStats());
  const sourceOnly = useAsyncData(() => DuplicateService.CountSourceOnlyRecords());
  const [sourceOnlyDismissed, setSourceOnlyDismissed] = useState(false);
  const [confirmRemoveSourceOnly, setConfirmRemoveSourceOnly] = useState(false);
  const [removingSourceOnly, setRemovingSourceOnly] = useState(false);
  const dupes = useAsyncData(() => DuplicateService.ListDuplicatesFiltered(filterDTO, page, PAGE_SIZE));
  const groups = useAsyncData(() =>
    filter.groupBy === "none"
      ? Promise.resolve([] as DuplicateGroupDTO[])
      : DuplicateService.ListDuplicateGroups(filter.groupBy),
  );

  const reloadAll = useCallback(() => {
    void stats.run({ silent: true }).catch(() => {});
    void dupes.run().catch((e) => toast.fromError(e, "Failed to load duplicates"));
    void groups.run({ silent: true }).catch(() => {});
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [page, filterDTO]);

  // Reload the page whenever filter/page changes.
  useEffect(() => {
    void dupes.run().catch((e) => toast.fromError(e, "Failed to load duplicates"));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [page, filterDTO]);

  // Reload the group picker when the group mode changes.
  useEffect(() => {
    void groups.run({ silent: true }).catch(() => {});
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [filter.groupBy]);

  // Header stats + source-only record count once on mount.
  useEffect(() => {
    void stats.run().catch(() => {});
    void sourceOnly.run().catch(() => {});
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const removeSourceOnly = async () => {
    setRemovingSourceOnly(true);
    try {
      const n = await DuplicateService.RemoveSourceOnlyRecords();
      toast.success(`Removed ${formatNumber(n)} already-imported record${n === 1 ? "" : "s"}`);
      setConfirmRemoveSourceOnly(false);
      await sourceOnly.run({ silent: true }).catch(() => {});
      reloadAll();
    } catch (e) {
      toast.fromError(e, "Could not remove records");
    } finally {
      setRemovingSourceOnly(false);
    }
  };

  // Re-attach to an in-flight bulk resolve on mount.
  useEffect(() => {
    void DuplicateService.ActiveBulkResolve()
      .then((active: ActiveBulkResolveDTO) => {
        if (active.state === "running") {
          setRunning(true);
          setProgress(active.progress ?? null);
        } else if (active.state === "completed") {
          setSummary(active.summary ?? null);
        }
      })
      .catch(() => {});
  }, []);

  // Track rendered item sizes so an explicit selection can total its wasted bytes.
  const items = dupes.data?.items ?? [];
  useEffect(() => {
    if (items.length === 0) return;
    setSizeById((prev) => {
      const next = new Map(prev);
      for (const p of items) {
        if (p.duplicate) next.set(p.duplicate.id, p.duplicate.fileSize ?? 0);
      }
      return next;
    });
  }, [items]);

  useWailsEvent<BulkResolveProgress>(WailsEvents.BulkResolveProgress, (p) => {
    setRunning(true);
    setProgress(p);
  });

  useWailsEvent<BulkResolveSummaryDTO>(WailsEvents.BulkResolveCompleted, (s) => {
    setRunning(false);
    setProgress(null);
    setSummary(s);
    // Clear the selection and refresh everything — the resolved rows are gone.
    setSelected(new Set());
    setAllInFilter(false);
    reloadAll();
  });

  // Live refresh when a background import completes (new duplicates may appear).
  useWailsEvent(WailsEvents.ImportCompleted, () => reloadAll());

  const totalPairs = stats.data?.totalPairs ?? 0;
  const totalWasted = stats.data?.totalWastedBytes ?? 0;
  const filteredTotal = dupes.data?.total ?? 0;
  const pageCount = Math.max(1, Math.ceil(filteredTotal / PAGE_SIZE));

  // The wasted bytes of the current filter: the selected group's figure when a
  // group is active, otherwise the archive-wide total.
  const activeGroup = groups.data?.find((g) => g.key === filter.groupKey);
  const filteredWasted =
    filter.groupBy !== "none" && filter.groupKey !== "" ? (activeGroup?.wastedBytes ?? 0) : totalWasted;

  const selectedCount = selected.size;
  const selectedBytes = allInFilter
    ? filteredWasted
    : Array.from(selected).reduce((sum, id) => sum + (sizeById.get(id) ?? 0), 0);

  const changeFilter = (next: Partial<Filter>) => {
    setFilter((f) => ({ ...f, ...next }));
    setPage(1);
    setSelected(new Set());
    setAllInFilter(false);
  };

  const toggleOne = (id: string) => {
    setAllInFilter(false);
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const allOnPageSelected = items.length > 0 && items.every((p) => selected.has(p.duplicate.id));
  const toggleSelectAllOnPage = () => {
    setAllInFilter(false);
    setSelected((prev) => {
      const next = new Set(prev);
      if (allOnPageSelected) {
        for (const p of items) next.delete(p.duplicate.id);
      } else {
        for (const p of items) next.add(p.duplicate.id);
      }
      return next;
    });
  };

  const selectAllInFilter = async () => {
    try {
      const ids = (await DuplicateService.ListDuplicateIDs(filterDTO)) ?? [];
      setSelected(new Set(ids));
      setAllInFilter(true);
    } catch (e) {
      toast.fromError(e, "Could not select all in filter");
    }
  };

  const clearSelection = () => {
    setSelected(new Set());
    setAllInFilter(false);
  };

  const filterLabel =
    filter.groupBy === "none"
      ? "the whole library"
      : filter.groupKey === ""
        ? filter.groupBy === "folder"
          ? "all folders"
          : "all sessions"
        : (activeGroup?.label ?? filter.groupKey);

  const askBatch = async (action: DupAction) => {
    const ids = Array.from(selected);
    if (ids.length === 0) return;

    let destFolder = "";
    if (action === "move") {
      try {
        destFolder = await CleanupService.PickFolder();
      } catch (e) {
        toast.fromError(e, "Could not open folder picker");
        return;
      }
      if (!destFolder) return; // cancelled
    }

    const count = ids.length;
    const bytes = selectedBytes;
    const common = { action, destFolder, ids, bytes };

    if (action === "delete") {
      setPending({
        ...common,
        title: `Delete ${formatNumber(count)} duplicates?`,
        confirmLabel: `Delete ${formatNumber(count)}`,
        variant: "danger",
        requireWord: "DELETE",
        description: (
          <span>
            Soft-delete <span className="font-medium text-zinc-200">{formatNumber(count)}</span> duplicate
            {count === 1 ? "" : "s"} (~<span className="font-medium text-amber-300">{formatBytes(bytes)}</span>) from{" "}
            <span className="font-medium text-zinc-200">{filterLabel}</span>. Each file is moved into{" "}
            <span className="font-mono text-zinc-300">.paim-trash/</span> inside your Master Library — never
            hard-deleted — and each record is flagged deleted. Originals are left untouched.
          </span>
        ),
      });
    } else if (action === "move") {
      setPending({
        ...common,
        title: `Move ${formatNumber(count)} duplicates?`,
        confirmLabel: `Move ${formatNumber(count)}`,
        variant: "primary",
        requireWord: "MOVE",
        description: (
          <span>
            Move <span className="font-medium text-zinc-200">{formatNumber(count)}</span> duplicate
            {count === 1 ? "" : "s"} to <span className="font-mono text-zinc-300">{destFolder}</span>. Each is a
            same-volume atomic rename, or a verified copy-then-trash across volumes. Archive records are updated to
            the new location.
          </span>
        ),
      });
    } else if (action === "ignore") {
      setPending({
        ...common,
        title: `Ignore ${formatNumber(count)} duplicates?`,
        confirmLabel: `Ignore ${formatNumber(count)}`,
        variant: "primary",
        description: (
          <span>
            Clear the duplicate flag on <span className="font-medium text-zinc-200">{formatNumber(count)}</span>{" "}
            asset{count === 1 ? "" : "s"} from <span className="font-medium text-zinc-200">{filterLabel}</span>. Both
            files in each pair are kept; these pairs no longer appear here.
          </span>
        ),
      });
    } else {
      setPending({
        ...common,
        title: `Keep both for ${formatNumber(count)} pairs?`,
        confirmLabel: `Keep both (${formatNumber(count)})`,
        variant: "primary",
        description: (
          <span>
            Mark <span className="font-medium text-zinc-200">{formatNumber(count)}</span> pair
            {count === 1 ? "" : "s"} as intentionally kept. The duplicate flag is cleared and a note recorded.
          </span>
        ),
      });
    }
  };

  const startBatch = async () => {
    if (!pending) return;
    setStarting(true);
    setSummary(null);
    try {
      await DuplicateService.StartBulkResolve(pending.ids, pending.action, pending.destFolder);
      setRunning(true);
      setProgress({
        action: pending.action,
        done: 0,
        total: pending.ids.length,
        succeeded: 0,
        failed: 0,
        currentFile: "",
      });
      setPending(null);
    } catch (e) {
      toast.fromError(e, "Could not start bulk resolve");
    } finally {
      setStarting(false);
    }
  };

  const cancelBatch = async () => {
    try {
      await DuplicateService.CancelBulkResolve();
    } catch (e) {
      toast.fromError(e, "Could not cancel");
    }
  };

  return (
    <div>
      <PageHeader
        title="Duplicate Manager"
        description="Full-hash-confirmed duplicate pairs. Filter, group, multi-select, and resolve in bulk — deletes are soft (moved to .paim-trash) and always confirmed."
        actions={
          <Button icon={ArrowPathIcon} onClick={reloadAll} loading={dupes.loading && !!dupes.data}>
            Refresh
          </Button>
        }
      />

      <div className="mb-5 grid grid-cols-2 gap-3 sm:max-w-md">
        <div className="rounded-lg border border-zinc-800 bg-zinc-900/60 p-4">
          <div className="text-xs font-medium text-zinc-500">Duplicate pairs</div>
          <div className="mt-2 text-2xl font-semibold tabular-nums text-zinc-100">{formatNumber(totalPairs)}</div>
        </div>
        <div className="rounded-lg border border-zinc-800 bg-zinc-900/60 p-4">
          <div className="text-xs font-medium text-zinc-500">Reclaimable (total)</div>
          <div className="mt-2 text-2xl font-semibold tabular-nums text-amber-400">{formatBytes(totalWasted)}</div>
        </div>
      </div>

      {!sourceOnlyDismissed && (sourceOnly.data ?? 0) > 0 ? (
        <SourceOnlyBanner
          count={sourceOnly.data ?? 0}
          busy={removingSourceOnly}
          onRemove={() => setConfirmRemoveSourceOnly(true)}
          onDismiss={() => setSourceOnlyDismissed(true)}
        />
      ) : null}

      {running || summary ? (
        <BulkRunPanel
          running={running}
          progress={progress}
          summary={summary}
          onCancel={() => void cancelBatch()}
          onDismiss={() => setSummary(null)}
        />
      ) : null}

      <FilterBar filter={filter} groups={groups.data ?? []} groupsLoading={groups.loading} onChange={changeFilter} />

      {dupes.loading && !dupes.data ? (
        <LoadingBlock label="Finding duplicates…" />
      ) : items.length === 0 ? (
        <Card>
          <EmptyState
            icon={DocumentDuplicateIcon}
            title="No duplicates"
            description="No full-hash-confirmed duplicate pairs match this filter. Duplicates flagged during import or adoption appear here."
          />
        </Card>
      ) : (
        <>
          <SelectionBar
            pageSelected={allOnPageSelected}
            onTogglePage={toggleSelectAllOnPage}
            selectedCount={selectedCount}
            selectedBytes={selectedBytes}
            filteredTotal={filteredTotal}
            filterLabel={filterLabel}
            allInFilter={allInFilter}
            busy={running}
            onSelectAllInFilter={() => void selectAllInFilter()}
            onClear={clearSelection}
            onAction={(a) => void askBatch(a)}
          />

          <div className="space-y-3">
            {items.map((pair) => (
              <DuplicatePairCard
                key={pair.duplicate.id}
                pair={pair}
                selected={selected.has(pair.duplicate.id)}
                onToggle={() => toggleOne(pair.duplicate.id)}
              />
            ))}

            {pageCount > 1 ? (
              <div className="flex items-center justify-between px-1 pt-1 text-xs text-zinc-500">
                <span className="tabular-nums">
                  Page {page} / {pageCount} · {formatNumber(filteredTotal)} pairs
                </span>
                <div className="flex items-center gap-2">
                  <Button size="sm" variant="secondary" disabled={page <= 1} onClick={() => setPage((p) => p - 1)}>
                    Previous
                  </Button>
                  <Button
                    size="sm"
                    variant="secondary"
                    disabled={page >= pageCount}
                    onClick={() => setPage((p) => p + 1)}
                  >
                    Next
                  </Button>
                </div>
              </div>
            ) : null}
          </div>
        </>
      )}

      <ConfirmDialog
        open={!!pending}
        title={pending?.title ?? ""}
        description={pending?.description}
        confirmLabel={pending?.confirmLabel}
        variant={pending?.variant ?? "primary"}
        requireWord={pending?.requireWord}
        loading={starting}
        onConfirm={() => void startBatch()}
        onCancel={() => (starting ? undefined : setPending(null))}
      />

      <ConfirmDialog
        open={confirmRemoveSourceOnly}
        title={`Remove ${formatNumber(sourceOnly.data ?? 0)} already-imported record${(sourceOnly.data ?? 0) === 1 ? "" : "s"}?`}
        description={
          <span>
            These <span className="font-medium text-zinc-200">{formatNumber(sourceOnly.data ?? 0)}</span> record
            {(sourceOnly.data ?? 0) === 1 ? "" : "s"} came from re-imports of sources whose photos were already
            archived. Nothing was copied, so they are not real duplicates — just already-imported entries. Removing
            them is record-only and reversible (rows are soft-deleted); no files are ever touched.
          </span>
        }
        confirmLabel="Remove records"
        variant="primary"
        loading={removingSourceOnly}
        onConfirm={() => void removeSourceOnly()}
        onCancel={() => (removingSourceOnly ? undefined : setConfirmRemoveSourceOnly(false))}
      />
    </div>
  );
}

/** One-time cleanup banner for legacy source-only "duplicate" records left by
 *  pre-v5 copy-mode re-imports of already-archived sources. */
function SourceOnlyBanner({
  count,
  busy,
  onRemove,
  onDismiss,
}: {
  count: number;
  busy: boolean;
  onRemove: () => void;
  onDismiss: () => void;
}) {
  return (
    <div className="mb-4 rounded-lg border border-blue-500/30 bg-blue-500/[0.04] p-4">
      <div className="flex items-start gap-3">
        <ExclamationTriangleIcon className="mt-0.5 h-5 w-5 flex-none text-blue-400" />
        <div className="min-w-0 flex-1">
          <div className="text-sm font-medium text-zinc-100">
            {formatNumber(count)} already-imported record{count === 1 ? "" : "s"} can be cleaned up
          </div>
          <p className="mt-1 text-xs leading-relaxed text-zinc-400">
            These came from re-imports of sources whose photos were already archived. Nothing was copied, so they
            are not real duplicates — the Duplicate Manager now only manages in-library copies. Removing them is
            record-only and reversible; no files are touched.
          </p>
        </div>
        <div className="flex flex-none items-center gap-2">
          <Button size="sm" variant="secondary" icon={TrashIcon} disabled={busy} onClick={onRemove}>
            Remove records
          </Button>
          <button
            type="button"
            onClick={onDismiss}
            className="text-zinc-500 hover:text-zinc-300"
            title="Dismiss"
          >
            <XMarkIcon className="h-4 w-4" />
          </button>
        </div>
      </div>
    </div>
  );
}

const ACTION_META: Record<DupAction, { label: string; icon: typeof TrashIcon; variant: "danger" | "secondary" }> = {
  delete: { label: "Delete", icon: TrashIcon, variant: "danger" },
  move: { label: "Move…", icon: FolderArrowDownIcon, variant: "secondary" },
  ignore: { label: "Ignore", icon: NoSymbolIcon, variant: "secondary" },
  keep_both: { label: "Keep both", icon: Square2StackIcon, variant: "secondary" },
};

/** Filter/group bar: group by folder/session, sort by size, and a group picker. */
function FilterBar({
  filter,
  groups,
  groupsLoading,
  onChange,
}: {
  filter: Filter;
  groups: DuplicateGroupDTO[];
  groupsLoading: boolean;
  onChange: (next: Partial<Filter>) => void;
}) {
  return (
    <div className="mb-4 space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-[11px] font-medium text-zinc-500">Group by</span>
        {(["none", "folder", "session"] as GroupBy[]).map((g) => (
          <button
            key={g}
            type="button"
            onClick={() => onChange({ groupBy: g, groupKey: "" })}
            className={`rounded-md px-2.5 py-1 text-xs font-medium transition ${
              filter.groupBy === g ? "bg-blue-600 text-white" : "bg-zinc-800/60 text-zinc-300 hover:bg-zinc-800"
            }`}
          >
            {g === "none" ? "None" : g === "folder" ? "Folder" : "Session"}
          </button>
        ))}

        <span className="mx-1 h-4 w-px bg-zinc-800" />

        <button
          type="button"
          onClick={() => onChange({ sortBySize: !filter.sortBySize })}
          className={`rounded-md px-2.5 py-1 text-xs font-medium transition ${
            filter.sortBySize ? "bg-blue-600 text-white" : "bg-zinc-800/60 text-zinc-300 hover:bg-zinc-800"
          }`}
          title="Order duplicates by reclaimable size, largest first"
        >
          Sort by size ↓
        </button>
      </div>

      {filter.groupBy !== "none" ? (
        <div className="rounded-lg border border-zinc-800 bg-zinc-900/40 p-2">
          <div className="mb-1.5 flex items-center justify-between px-1">
            <span className="text-[11px] font-medium text-zinc-500">
              {filter.groupBy === "folder" ? "Folders" : "Import sessions"} with duplicates
            </span>
            {filter.groupKey !== "" ? (
              <button
                type="button"
                onClick={() => onChange({ groupKey: "" })}
                className="text-[11px] text-zinc-400 hover:text-zinc-200"
              >
                Show all
              </button>
            ) : null}
          </div>
          {groupsLoading && groups.length === 0 ? (
            <div className="px-1 py-2 text-xs text-zinc-500">Loading groups…</div>
          ) : groups.length === 0 ? (
            <div className="px-1 py-2 text-xs text-zinc-500">No groups.</div>
          ) : (
            <div className="max-h-56 space-y-0.5 overflow-y-auto">
              {groups.map((g) => (
                <button
                  key={g.key}
                  type="button"
                  onClick={() => onChange({ groupKey: filter.groupKey === g.key ? "" : g.key })}
                  className={`flex w-full items-center justify-between gap-3 rounded-md px-2 py-1.5 text-left text-xs transition ${
                    filter.groupKey === g.key ? "bg-blue-600/20 ring-1 ring-blue-500/40" : "hover:bg-zinc-800/60"
                  }`}
                >
                  <span className="min-w-0 flex-1 truncate font-mono text-zinc-300" title={g.label}>
                    {g.label}
                  </span>
                  <span className="flex-none tabular-nums text-zinc-500">{formatNumber(g.count)} pairs</span>
                  <span className="w-20 flex-none text-right tabular-nums text-amber-400">
                    {formatBytes(g.wastedBytes)}
                  </span>
                </button>
              ))}
            </div>
          )}
        </div>
      ) : null}
    </div>
  );
}

/** Selection toolbar: page select-all, counts, select-all-in-filter, batch actions. */
function SelectionBar({
  pageSelected,
  onTogglePage,
  selectedCount,
  selectedBytes,
  filteredTotal,
  filterLabel,
  allInFilter,
  busy,
  onSelectAllInFilter,
  onClear,
  onAction,
}: {
  pageSelected: boolean;
  onTogglePage: () => void;
  selectedCount: number;
  selectedBytes: number;
  filteredTotal: number;
  filterLabel: string;
  allInFilter: boolean;
  busy: boolean;
  onSelectAllInFilter: () => void;
  onClear: () => void;
  onAction: (a: DupAction) => void;
}) {
  const has = selectedCount > 0;
  return (
    <div className="mb-3 flex flex-wrap items-center gap-2 rounded-lg border border-zinc-800 bg-zinc-900/60 px-3 py-2">
      <label className="flex cursor-pointer items-center gap-2 text-xs text-zinc-300">
        <input
          type="checkbox"
          checked={pageSelected}
          onChange={onTogglePage}
          className="h-4 w-4 rounded border-zinc-600 bg-zinc-800 text-blue-600"
        />
        Select page
      </label>

      {filteredTotal > 0 ? (
        <button
          type="button"
          onClick={onSelectAllInFilter}
          className="text-xs text-blue-400 hover:text-blue-300 disabled:opacity-50"
          disabled={busy}
          title={`Select all ${formatNumber(filteredTotal)} duplicates in ${filterLabel}`}
        >
          Select all {formatNumber(filteredTotal)} in {filterLabel}
        </button>
      ) : null}

      <div className="flex-1" />

      {has ? (
        <>
          <span className="text-xs tabular-nums text-zinc-400">
            {formatNumber(selectedCount)} selected · {allInFilter ? "" : "~"}
            <span className="text-amber-300">{formatBytes(selectedBytes)}</span>
          </span>
          <button type="button" onClick={onClear} className="text-zinc-500 hover:text-zinc-300" title="Clear selection">
            <XMarkIcon className="h-4 w-4" />
          </button>
          <span className="mx-1 h-4 w-px bg-zinc-800" />
          <span className="text-[11px] text-zinc-500">Resolve selected:</span>
          {(Object.keys(ACTION_META) as DupAction[]).map((a) => {
            const meta = ACTION_META[a];
            return (
              <Button
                key={a}
                size="sm"
                variant={meta.variant}
                icon={meta.icon}
                disabled={busy}
                onClick={() => onAction(a)}
              >
                {meta.label}
              </Button>
            );
          })}
        </>
      ) : (
        <span className="text-xs text-zinc-500">Select duplicates to resolve them in bulk.</span>
      )}
    </div>
  );
}

/** Progress + completion panel for a running/finished bulk resolve. */
function BulkRunPanel({
  running,
  progress,
  summary,
  onCancel,
  onDismiss,
}: {
  running: boolean;
  progress: BulkResolveProgress | null;
  summary: BulkResolveSummaryDTO | null;
  onCancel: () => void;
  onDismiss: () => void;
}) {
  if (running) {
    const done = progress?.done ?? 0;
    const total = progress?.total ?? 0;
    const pct = total > 0 ? Math.round((done / total) * 100) : null;
    return (
      <div className="mb-4 rounded-lg border border-blue-500/30 bg-blue-500/[0.04] p-4">
        <div className="mb-2 flex items-center justify-between">
          <span className="text-sm font-medium text-zinc-100">Resolving duplicates…</span>
          <Button size="sm" variant="secondary" onClick={onCancel}>
            Cancel
          </Button>
        </div>
        <ProgressBar
          percent={pct}
          tone="accent"
          label={
            <span className="tabular-nums">
              {formatNumber(done)} of {formatNumber(total)} · {formatNumber(progress?.succeeded ?? 0)} done
              {progress?.failed ? ` · ${formatNumber(progress.failed)} failed` : ""}
            </span>
          }
        />
        {progress?.currentFile ? (
          <div className="mt-1 truncate font-mono text-[11px] text-zinc-500" title={progress.currentFile}>
            {progress.currentFile}
          </div>
        ) : null}
      </div>
    );
  }

  if (!summary) return null;
  const tone =
    summary.failed > 0 ? "border-amber-500/40 bg-amber-500/[0.04]" : "border-emerald-500/30 bg-emerald-500/[0.04]";
  const failures = summary.failures ?? [];
  return (
    <div className={`mb-4 rounded-lg border p-4 ${tone}`}>
      <div className="mb-1 flex items-center justify-between">
        <span className="text-sm font-medium text-zinc-100">
          {summary.cancelled ? "Bulk resolve cancelled" : "Bulk resolve complete"}
        </span>
        <button type="button" onClick={onDismiss} className="text-zinc-500 hover:text-zinc-300">
          <XMarkIcon className="h-4 w-4" />
        </button>
      </div>
      <div className="text-xs tabular-nums text-zinc-400">
        {formatNumber(summary.succeeded)} resolved
        {summary.failed > 0 ? <span className="text-amber-300"> · {formatNumber(summary.failed)} failed</span> : null}
        {summary.cancelled ? <span className="text-zinc-500"> · stopped early (remaining left flagged)</span> : null}
      </div>
      {failures.length > 0 ? (
        <div className="mt-3 max-h-40 space-y-1 overflow-y-auto rounded-md border border-zinc-800 bg-zinc-950/40 p-2">
          {failures.map((f) => (
            <div key={f.assetId} className="text-[11px]">
              <span className="font-medium text-zinc-300">{f.filename || f.assetId}</span>
              <span className="text-zinc-600"> — </span>
              <span className="text-amber-300/80">{f.error}</span>
            </div>
          ))}
        </div>
      ) : null}
    </div>
  );
}

function DuplicatePairCard({
  pair,
  selected,
  onToggle,
}: {
  pair: DuplicatePairDTO;
  selected: boolean;
  onToggle: () => void;
}) {
  const toast = useToast();

  const reveal = async (assetId: string, which: "archive" | "original") => {
    try {
      await BrowserService.RevealAsset(assetId, which);
    } catch (e) {
      toast.fromError(e, "Could not reveal in Finder");
    }
  };

  return (
    <Card className={selected ? "ring-1 ring-blue-500/50" : undefined}>
      <div className="mb-3 flex items-center gap-2 border-b border-zinc-800 pb-3">
        <input
          type="checkbox"
          checked={selected}
          onChange={onToggle}
          className="h-4 w-4 rounded border-zinc-600 bg-zinc-800 text-blue-600"
          aria-label="Select this duplicate"
        />
        <span className="text-[11px] text-zinc-500">
          {selected ? "Selected for bulk resolve" : "Select to include in a bulk action"}
        </span>
      </div>

      <div className="grid gap-4 md:grid-cols-2">
        <AssetColumn
          asset={pair.duplicate}
          kind="duplicate"
          fileExists={pair.duplicateFileExists}
          revealLabel="Reveal in archive"
          onReveal={() => void reveal(pair.duplicate.id, "archive")}
        />
        <AssetColumn
          asset={pair.original}
          kind="original"
          fileExists={pair.originalFileExists}
          revealLabel="Reveal in archive"
          onReveal={() => void reveal(pair.original.id, "archive")}
        />
      </div>
    </Card>
  );
}

function AssetColumn({
  asset,
  kind,
  fileExists,
  revealLabel,
  onReveal,
}: {
  asset: AssetDTO;
  kind: "duplicate" | "original";
  fileExists: boolean;
  revealLabel: string;
  onReveal: () => void;
}) {
  // Every managed duplicate/original has its own archived file.
  const path = asset.currentArchivePath || asset.originalFullPath;
  return (
    <div
      className={`rounded-lg border p-3 ${
        kind === "duplicate" ? "border-amber-500/30 bg-amber-500/[0.03]" : "border-zinc-800 bg-zinc-950/40"
      }`}
    >
      <div className="mb-2 flex flex-wrap items-center gap-1.5">
        {kind === "duplicate" ? (
          <StatusBadge status="duplicate" tone="warn" label="Duplicate" dot />
        ) : (
          <StatusBadge status="original" tone="success" label="Original" dot />
        )}
        {kind === "duplicate" ? <ArrowRightCircleIcon className="h-4 w-4 text-zinc-600" /> : null}
        {!fileExists ? (
          <span
            className="inline-flex items-center gap-1 rounded-full border border-zinc-700 bg-zinc-800/60 px-2 py-0.5 text-[10px] font-medium text-zinc-400"
            title="This file is not reachable right now — the source drive may be ejected or the file was moved."
          >
            Unavailable
          </span>
        ) : null}
      </div>

      <div className="flex gap-3">
        <DupThumb asset={asset} />
        <div className="min-w-0 flex-1">
          <div className="truncate text-[13px] font-medium text-zinc-100" title={asset.originalFilename}>
            {asset.originalFilename}
          </div>
          <PathText path={path} />

          <div className="mt-3 grid grid-cols-2 gap-x-4 gap-y-1 text-[11px]">
            <Field label="Size" value={formatBytes(asset.fileSize)} />
            <Field label="Imported" value={formatDate(asset.importDate)} />
          </div>
        </div>
      </div>

      <div className="mt-2 space-y-1">
        <HashRow label="Quick" hash={asset.quickHash} />
        <HashRow label="Full" hash={asset.fullHash} />
      </div>

      <div className="mt-2">
        <Button
          size="sm"
          variant="ghost"
          icon={FolderOpenIcon}
          disabled={!fileExists}
          title={fileExists ? "Reveal this file in Finder" : "File not available right now — cannot reveal"}
          onClick={onReveal}
        >
          {revealLabel}
        </Button>
      </div>
    </div>
  );
}

/**
 * PathText renders a full path that never truncates un-recoverably: it wraps
 * (break-all) and is clamped to two lines by default, expanding to the full path
 * on click. The title tooltip always carries the complete path too.
 */
function PathText({ path }: { path: string }) {
  const [expanded, setExpanded] = useState(false);
  if (!path) {
    return <div className="mt-0.5 font-mono text-[11px] text-zinc-600">—</div>;
  }
  return (
    <button
      type="button"
      onClick={() => setExpanded((v) => !v)}
      title={expanded ? "Click to collapse" : path}
      className={`selectable mt-0.5 block w-full text-left font-mono text-[11px] break-all text-zinc-500 transition hover:text-zinc-300 ${
        expanded ? "" : "line-clamp-2"
      }`}
    >
      {path}
    </button>
  );
}

/** Grid-size thumbnail beside a duplicate/original, with placeholder fallback. */
function DupThumb({ asset }: { asset: AssetDTO }) {
  const [errored, setErrored] = useState(false);
  return (
    <div className="h-24 w-24 flex-none overflow-hidden rounded-md border border-zinc-800 bg-zinc-900">
      {errored ? (
        <div className="flex h-full w-full items-center justify-center">
          <PhotoIcon className="h-6 w-6 text-zinc-700" />
        </div>
      ) : (
        <img
          src={`/thumb/${asset.id}`}
          loading="lazy"
          alt={asset.originalFilename}
          onError={() => setErrored(true)}
          className="h-full w-full object-cover"
        />
      )}
    </div>
  );
}

function Field({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center justify-between gap-2">
      <span className="text-zinc-600">{label}</span>
      <span className="truncate text-zinc-300" title={value}>
        {value}
      </span>
    </div>
  );
}

function HashRow({ label, hash }: { label: string; hash: string }) {
  const toast = useToast();
  const [copied, setCopied] = useState(false);
  if (!hash) {
    return (
      <div className="flex items-center gap-2 text-[10px] text-zinc-600">
        <span className="w-9 text-zinc-600">{label}</span>
        <span>not computed</span>
      </div>
    );
  }
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(hash);
      setCopied(true);
      setTimeout(() => setCopied(false), 1200);
    } catch {
      toast.warn("Could not copy to clipboard");
    }
  };
  return (
    <button
      onClick={() => void copy()}
      className="group flex w-full items-center gap-2 text-left text-[10px] text-zinc-500 transition hover:text-zinc-300"
      title={`${hash} — click to copy`}
    >
      <span className="w-9 flex-none text-zinc-600">{label}</span>
      <span className="truncate font-mono">{hash.slice(0, 16)}…</span>
      {copied ? (
        <CheckIcon className="h-3 w-3 flex-none text-emerald-400" />
      ) : (
        <ClipboardDocumentIcon className="h-3 w-3 flex-none opacity-0 transition group-hover:opacity-100" />
      )}
    </button>
  );
}
