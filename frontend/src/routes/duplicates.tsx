import { useEffect, useState } from "react";
import {
  ArrowPathIcon,
  ArrowRightCircleIcon,
  CheckIcon,
  ClipboardDocumentIcon,
  DocumentDuplicateIcon,
  FolderArrowDownIcon,
  NoSymbolIcon,
  PhotoIcon,
  Square2StackIcon,
  TrashIcon,
} from "@heroicons/react/24/outline";
import {
  Button,
  Card,
  ConfirmDialog,
  EmptyState,
  LoadingBlock,
  PageHeader,
  StatusBadge,
} from "../components";
import {
  CleanupService,
  DuplicateService,
  WailsEvents,
  type AssetDTO,
  type DuplicatePairDTO,
  type DuplicateProgress,
} from "../lib/api";
import { useAsyncData, useWailsEvent } from "../lib/hooks";
import { useToast } from "../lib/toast";
import { formatBytes, formatDate, formatNumber } from "../lib/format";

const PAGE_SIZE = 20;

type DupAction = "delete" | "move" | "ignore" | "keep_both";

interface PendingAction {
  duplicateAssetID: string;
  action: DupAction;
  destFolder: string;
  title: string;
  description: React.ReactNode;
  confirmLabel: string;
  variant: "danger" | "primary";
  requireWord?: string;
}

/** Duplicate Manager — full-hash-confirmed pairs with safe, always-confirmed actions. */
export function DuplicatesPage() {
  const toast = useToast();
  const [page, setPage] = useState(1);
  const dupes = useAsyncData(() => DuplicateService.ListDuplicates(page, PAGE_SIZE));
  const [pending, setPending] = useState<PendingAction | null>(null);
  const [working, setWorking] = useState(false);
  // Byte progress for a cross-volume move (copy+verify), shown in the dialog.
  const [moveProgress, setMoveProgress] = useState<DuplicateProgress | null>(null);

  useWailsEvent<DuplicateProgress>(WailsEvents.DuplicateProgress, (p) => setMoveProgress(p));

  useEffect(() => {
    void dupes.run().catch((e) => toast.fromError(e, "Failed to load duplicates"));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [page]);

  const items = dupes.data?.items ?? [];
  const total = dupes.data?.total ?? 0;
  const pageCount = Math.max(1, Math.ceil(total / PAGE_SIZE));
  const wastedBytes = items.reduce((sum, p) => sum + (p.duplicate?.fileSize ?? 0), 0);

  const refresh = () => dupes.run({ silent: true });

  const runAction = async () => {
    if (!pending) return;
    setWorking(true);
    setMoveProgress(null);
    try {
      await DuplicateService.ResolveDuplicate(pending.duplicateAssetID, pending.action, pending.destFolder);
      toast.success(ACTION_DONE_LABEL[pending.action]);
      setPending(null);
      await refresh();
    } catch (e) {
      toast.fromError(e, "Could not resolve duplicate");
    } finally {
      setWorking(false);
      setMoveProgress(null);
    }
  };

  const askDelete = (dup: AssetDTO) =>
    setPending({
      duplicateAssetID: dup.id,
      action: "delete",
      destFolder: "",
      title: "Delete this duplicate?",
      variant: "danger",
      confirmLabel: "Delete duplicate",
      requireWord: "DELETE",
      description: (
        <span>
          The duplicate file <span className="font-medium text-zinc-200">{dup.originalFilename}</span> will be soft-deleted:
          its record is flagged deleted and the physical file is moved into{" "}
          <span className="font-mono text-zinc-300">.paim-trash/</span> inside your Master Library. It is never
          hard-deleted, and the original is left untouched.
        </span>
      ),
    });

  const askMove = async (dup: AssetDTO) => {
    let dest = "";
    try {
      dest = await CleanupService.PickFolder();
    } catch (e) {
      toast.fromError(e, "Could not open folder picker");
      return;
    }
    if (!dest) return; // user cancelled
    setPending({
      duplicateAssetID: dup.id,
      action: "move",
      destFolder: dest,
      title: "Move this duplicate?",
      variant: "primary",
      confirmLabel: "Move duplicate",
      description: (
        <span>
          Move <span className="font-medium text-zinc-200">{dup.originalFilename}</span> to{" "}
          <span className="font-mono text-zinc-300">{dest}</span>. This is a same-volume atomic rename, or a verified
          copy-then-trash across volumes. The archive record is updated to the new location.
        </span>
      ),
    });
  };

  const askIgnore = (dup: AssetDTO) =>
    setPending({
      duplicateAssetID: dup.id,
      action: "ignore",
      destFolder: "",
      title: "Ignore this duplicate?",
      variant: "primary",
      confirmLabel: "Ignore",
      description: (
        <span>
          Clears the duplicate flag on <span className="font-medium text-zinc-200">{dup.originalFilename}</span> and
          records a note. Both files are kept; this pair will no longer appear here.
        </span>
      ),
    });

  const askKeepBoth = (dup: AssetDTO) =>
    setPending({
      duplicateAssetID: dup.id,
      action: "keep_both",
      destFolder: "",
      title: "Keep both files?",
      variant: "primary",
      confirmLabel: "Keep both",
      description: (
        <span>
          Marks that you intentionally keep both{" "}
          <span className="font-medium text-zinc-200">{dup.originalFilename}</span> copies. The duplicate flag is
          cleared and a note is recorded.
        </span>
      ),
    });

  return (
    <div>
      <PageHeader
        title="Duplicate Manager"
        description="Full-hash-confirmed duplicate pairs. Every action is safe — deletes are soft (moved to .paim-trash) and always confirmed."
        actions={
          <Button
            icon={ArrowPathIcon}
            onClick={() => void dupes.run().catch((e) => toast.fromError(e, "Failed to load duplicates"))}
            loading={dupes.loading && !!dupes.data}
          >
            Refresh
          </Button>
        }
      />

      <div className="mb-5 grid grid-cols-2 gap-3 sm:max-w-md">
        <div className="rounded-lg border border-zinc-800 bg-zinc-900/60 p-4">
          <div className="text-xs font-medium text-zinc-500">Duplicate pairs</div>
          <div className="mt-2 text-2xl font-semibold tabular-nums text-zinc-100">{formatNumber(total)}</div>
        </div>
        <div className="rounded-lg border border-zinc-800 bg-zinc-900/60 p-4">
          <div className="text-xs font-medium text-zinc-500">Wasted (this page)</div>
          <div className="mt-2 text-2xl font-semibold tabular-nums text-amber-400">{formatBytes(wastedBytes)}</div>
        </div>
      </div>

      {dupes.loading && !dupes.data ? (
        <LoadingBlock label="Finding duplicates…" />
      ) : items.length === 0 ? (
        <Card>
          <EmptyState
            icon={DocumentDuplicateIcon}
            title="No duplicates"
            description="No full-hash-confirmed duplicate pairs were found. Duplicates flagged during import or adoption appear here."
          />
        </Card>
      ) : (
        <div className="space-y-3">
          {items.map((pair) => (
            <DuplicatePairCard
              key={pair.duplicate.id}
              pair={pair}
              onDelete={() => askDelete(pair.duplicate)}
              onMove={() => void askMove(pair.duplicate)}
              onIgnore={() => askIgnore(pair.duplicate)}
              onKeepBoth={() => askKeepBoth(pair.duplicate)}
            />
          ))}

          {pageCount > 1 ? (
            <div className="flex items-center justify-between px-1 pt-1 text-xs text-zinc-500">
              <span className="tabular-nums">
                Page {page} / {pageCount} · {formatNumber(total)} pairs
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
      )}

      <ConfirmDialog
        open={!!pending}
        title={pending?.title ?? ""}
        description={pending?.description}
        confirmLabel={pending?.confirmLabel}
        variant={pending?.variant ?? "primary"}
        requireWord={pending?.requireWord}
        loading={working}
        loadingContent={
          moveProgress && moveProgress.bytesTotal > 0 ? (
            <div>
              <div className="mb-1 flex items-center justify-between text-[11px] text-zinc-500">
                <span>Copying across volumes…</span>
                <span className="tabular-nums">
                  {formatBytes(moveProgress.bytesDone)} / {formatBytes(moveProgress.bytesTotal)}
                </span>
              </div>
              <div className="h-1.5 w-full overflow-hidden rounded-full bg-zinc-800">
                <div
                  className="h-full rounded-full bg-blue-500 transition-all"
                  style={{ width: `${Math.min(100, Math.round((moveProgress.bytesDone / moveProgress.bytesTotal) * 100))}%` }}
                />
              </div>
            </div>
          ) : null
        }
        onConfirm={() => void runAction()}
        onCancel={() => (working ? undefined : setPending(null))}
      />
    </div>
  );
}

const ACTION_DONE_LABEL: Record<DupAction, string> = {
  delete: "Duplicate deleted",
  move: "Duplicate moved",
  ignore: "Duplicate ignored",
  keep_both: "Keeping both files",
};

function DuplicatePairCard({
  pair,
  onDelete,
  onMove,
  onIgnore,
  onKeepBoth,
}: {
  pair: DuplicatePairDTO;
  onDelete: () => void;
  onMove: () => void;
  onIgnore: () => void;
  onKeepBoth: () => void;
}) {
  return (
    <Card>
      <div className="grid gap-4 md:grid-cols-2">
        <AssetColumn asset={pair.duplicate} kind="duplicate" />
        <AssetColumn asset={pair.original} kind="original" />
      </div>

      <div className="mt-4 flex flex-wrap items-center gap-2 border-t border-zinc-800 pt-3">
        <span className="mr-1 text-[11px] text-zinc-500">Resolve the duplicate:</span>
        <Button size="sm" variant="danger" icon={TrashIcon} onClick={onDelete}>
          Delete
        </Button>
        <Button size="sm" variant="secondary" icon={FolderArrowDownIcon} onClick={onMove}>
          Move…
        </Button>
        <Button size="sm" variant="secondary" icon={NoSymbolIcon} onClick={onIgnore}>
          Ignore
        </Button>
        <Button size="sm" variant="secondary" icon={Square2StackIcon} onClick={onKeepBoth}>
          Keep both
        </Button>
      </div>
    </Card>
  );
}

function AssetColumn({ asset, kind }: { asset: AssetDTO; kind: "duplicate" | "original" }) {
  return (
    <div
      className={`rounded-lg border p-3 ${
        kind === "duplicate" ? "border-amber-500/30 bg-amber-500/[0.03]" : "border-zinc-800 bg-zinc-950/40"
      }`}
    >
      <div className="mb-2 flex items-center gap-2">
        {kind === "duplicate" ? (
          <StatusBadge status="duplicate" tone="warn" label="Duplicate" dot />
        ) : (
          <StatusBadge status="original" tone="success" label="Original" dot />
        )}
        {kind === "duplicate" ? <ArrowRightCircleIcon className="h-4 w-4 text-zinc-600" /> : null}
      </div>

      <div className="flex gap-3">
        <DupThumb asset={asset} />
        <div className="min-w-0 flex-1">
          <div className="truncate text-[13px] font-medium text-zinc-100" title={asset.originalFilename}>
            {asset.originalFilename}
          </div>
          <div
            className="selectable mt-0.5 truncate font-mono text-[11px] text-zinc-500"
            title={asset.currentArchivePath || asset.originalFullPath}
          >
            {asset.currentArchivePath || asset.originalFullPath || "—"}
          </div>

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
    </div>
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
