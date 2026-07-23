import { useEffect, useState } from "react";
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
  BrowserService,
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
  const toast = useToast();

  const reveal = async (assetId: string, which: "archive" | "original") => {
    try {
      await BrowserService.RevealAsset(assetId, which);
    } catch (e) {
      toast.fromError(e, "Could not reveal in Finder");
    }
  };

  // The duplicate's file lives at its archive copy (adopt mode) or — for a
  // copy-mode duplicate that was flagged but never copied — only at its source.
  const dupSourceOnly = !pair.duplicateHasArchiveCopy;
  const dupWhich = pair.duplicateHasArchiveCopy ? "archive" : "original";

  return (
    <Card>
      <div className="grid gap-4 md:grid-cols-2">
        <AssetColumn
          asset={pair.duplicate}
          kind="duplicate"
          sourceOnly={dupSourceOnly}
          fileExists={pair.duplicateFileExists}
          revealLabel={dupSourceOnly ? "Reveal at source" : "Reveal in archive"}
          onReveal={() => void reveal(pair.duplicate.id, dupWhich)}
        />
        <AssetColumn
          asset={pair.original}
          kind="original"
          sourceOnly={false}
          fileExists={pair.originalFileExists}
          revealLabel="Reveal in archive"
          onReveal={() => void reveal(pair.original.id, "archive")}
        />
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

function AssetColumn({
  asset,
  kind,
  sourceOnly,
  fileExists,
  revealLabel,
  onReveal,
}: {
  asset: AssetDTO;
  kind: "duplicate" | "original";
  sourceOnly: boolean;
  fileExists: boolean;
  revealLabel: string;
  onReveal: () => void;
}) {
  // The file's current location: an archive copy if one exists, otherwise the
  // original source path (an SD card / drive for a copy-mode duplicate).
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
        {sourceOnly ? (
          <span
            className="inline-flex items-center gap-1 rounded-full border border-amber-500/40 bg-amber-500/10 px-2 py-0.5 text-[10px] font-semibold text-amber-300"
            title="PAIM flags duplicates without re-copying them, so this file was never written into your archive. It lives only at its source path above — which may become unreachable once that SD card or drive is ejected."
          >
            <ExclamationTriangleIcon className="h-3 w-3" />
            On source only — never copied
          </span>
        ) : null}
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

      {sourceOnly ? (
        <p className="mt-2 text-[11px] leading-relaxed text-amber-300/80">
          Recorded as a duplicate without copying — the file above may be unreachable once its source is ejected.
        </p>
      ) : null}

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
