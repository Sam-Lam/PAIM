import { useEffect, useState } from "react";
import { TrashIcon, CheckCircleIcon, ArrowUpTrayIcon } from "@heroicons/react/24/outline";
import { Button } from "./Button";
import { ConfirmDialog } from "./ConfirmDialog";
import {
  SourcesService,
  WailsEvents,
  type ActiveClearSourceDTO,
  type ClearSourcePreviewDTO,
  type SafeToEraseDTO,
  type SourceCleared,
  type SourceProgress,
} from "../lib/api";
import { useWailsEvent } from "../lib/hooks";
import { useToast } from "../lib/toast";
import { formatBytes, formatNumber } from "../lib/format";

/**
 * ClearSourceControl offers the gated "Clear imported media…" affordance shared
 * by the Sources page (on a green evaluation) and the Import completion panel.
 *
 * It never decides safety itself: the parent passes the current safe-to-erase
 * `report` for `root` and whether that evaluation is `fresh` (this root, current).
 * The button is enabled only on a green + fresh report; the backend re-validates
 * the gate again on preview and on start, so a lapsed evaluation is refused
 * server-side regardless of UI state. Moving files never deletes — they go to
 * <root>/.paim-trash/<timestamp>/ preserving their relative paths.
 */
export function ClearSourceControl({
  root,
  report,
  fresh,
}: {
  root: string;
  report: SafeToEraseDTO | null;
  fresh: boolean;
}) {
  const toast = useToast();
  const [clear, setClear] = useState<ActiveClearSourceDTO | null>(null);
  const [preview, setPreview] = useState<ClearSourcePreviewDTO | null>(null);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [starting, setStarting] = useState(false);

  // Re-attach to a running/completed clear for this root on mount.
  useEffect(() => {
    void SourcesService.ActiveClearSource()
      .then((dto) => {
        if (dto.state !== "none" && dto.root === root) setClear(dto);
      })
      .catch(() => undefined);
  }, [root]);

  useWailsEvent<SourceProgress>(WailsEvents.SourceProgress, (p) => {
    if (p.kind === "clear" && p.mountPoint === root) {
      setClear({ state: "running", root, progress: p, result: null, cancelled: false, error: "" });
    }
  });
  useWailsEvent<SourceCleared>(WailsEvents.SourceCleared, (e) => {
    if (e.root !== root) return;
    setClear({
      state: "completed",
      root,
      progress: null,
      result: {
        root: e.root,
        trashDir: e.trashDir,
        moved: e.moved,
        skippedUnsafe: e.skippedUnsafe,
        errors: e.errors,
        cancelled: e.cancelled,
      },
      cancelled: e.cancelled,
      error: "",
    });
  });

  const running = clear?.state === "running";
  const canClear = !!report?.safe && fresh && !running && !starting;

  const openConfirm = async () => {
    try {
      const pv = await SourcesService.ClearSourcePreview(root);
      setPreview(pv);
      setConfirmOpen(true);
    } catch (e) {
      // A lapsed/absent/red gate surfaces the backend's "evaluate first" message.
      toast.fromError(e, "Cannot clear yet");
    }
  };

  const startClear = async () => {
    setStarting(true);
    try {
      await SourcesService.StartClearSource(root);
      setConfirmOpen(false);
    } catch (e) {
      toast.fromError(e, "Could not start clearing");
    } finally {
      setStarting(false);
    }
  };

  const cancelClear = () => {
    void SourcesService.CancelClearSource().catch(() => undefined);
  };

  const [ejecting, setEjecting] = useState(false);
  // Offer to eject the just-cleared source. The backend resolves the volume from
  // the root and re-checks every safety guard; the OS refuses a busy disk.
  const eject = async () => {
    setEjecting(true);
    try {
      await SourcesService.EjectVolume(root);
      toast.success("Ejected", "The source is safe to unplug.");
    } catch (e) {
      toast.fromError(e, "Could not eject");
    } finally {
      setEjecting(false);
    }
  };

  return (
    <div className="mt-3">
      {!running && clear?.state !== "completed" ? (
        <Button icon={TrashIcon} variant="danger" onClick={openConfirm} disabled={!canClear}>
          Clear imported media…
        </Button>
      ) : null}

      {running ? <ClearProgress progress={clear?.progress ?? null} onCancel={cancelClear} /> : null}

      {clear?.state === "completed" && clear.result ? (
        <ClearSummary result={clear.result} onEject={() => void eject()} ejecting={ejecting} />
      ) : null}

      <ConfirmDialog
        open={confirmOpen}
        title="Clear the imported source?"
        requireWord="CLEAR"
        confirmLabel="Clear imported media"
        cancelLabel="Keep files"
        loading={starting}
        description={
          preview ? (
            <div className="space-y-2">
              <p>
                This moves <span className="font-semibold text-zinc-200">{formatNumber(preview.fileCount)}</span> safe,
                fully backed-up file{preview.fileCount === 1 ? "" : "s"}
                {preview.totalBytes > 0 ? (
                  <>
                    {" "}(<span className="font-semibold text-zinc-200">{formatBytes(preview.totalBytes)}</span>)
                  </>
                ) : null}{" "}
                into a timestamped folder under:
              </p>
              <p className="selectable break-all rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1 font-mono text-[11px] text-zinc-300">
                {preview.trashDir}/
              </p>
              <p>
                PAIM never deletes — nothing leaves the drive. Files that are not safe to erase are left untouched. You
                can format the card or empty the trash folder yourself afterward.
              </p>
            </div>
          ) : null
        }
        onConfirm={startClear}
        onCancel={() => setConfirmOpen(false)}
      />
    </div>
  );
}

function ClearProgress({ progress, onCancel }: { progress: SourceProgress | null; onCancel: () => void }) {
  const done = progress?.filesDone ?? 0;
  const total = progress?.filesTotal ?? 0;
  const pct = total > 0 ? Math.min(100, Math.round((done / total) * 100)) : 0;
  return (
    <div className="rounded-lg border border-zinc-800 bg-zinc-950/40 p-3">
      <div className="mb-2 flex items-center justify-between">
        <h4 className="text-xs font-semibold text-zinc-300">Clearing source…</h4>
        <button className="text-[11px] text-blue-400 hover:underline" onClick={onCancel}>
          Cancel
        </button>
      </div>
      <div className="h-2 w-full overflow-hidden rounded-full bg-zinc-800">
        <div className="h-full rounded-full bg-blue-500 transition-all" style={{ width: `${pct}%` }} />
      </div>
      <div className="mt-1 flex items-center justify-between text-[10px] text-zinc-500">
        <span className="tabular-nums">
          {formatNumber(done)} of {formatNumber(total)} moved
        </span>
        <span>{pct}%</span>
      </div>
      {progress?.currentFile ? (
        <p className="selectable mt-1 truncate text-[10px] text-zinc-600" title={progress.currentFile}>
          {progress.currentFile}
        </p>
      ) : null}
    </div>
  );
}

function ClearSummary({
  result,
  onEject,
  ejecting,
}: {
  result: NonNullable<ActiveClearSourceDTO["result"]>;
  onEject: () => void;
  ejecting: boolean;
}) {
  return (
    <div className="rounded-lg border border-emerald-800/40 bg-emerald-950/20 p-3">
      <div className="mb-1 flex items-center gap-2">
        <CheckCircleIcon className="h-4 w-4 text-emerald-400" />
        <h4 className="text-xs font-semibold text-zinc-200">
          {result.cancelled ? "Clearing cancelled" : "Source cleared"}
        </h4>
      </div>
      <p className="text-[12px] leading-relaxed text-zinc-400">
        Moved <span className="font-semibold text-zinc-200">{formatNumber(result.moved)}</span> file
        {result.moved === 1 ? "" : "s"} to trash.
        {result.errors > 0 ? (
          <span className="text-amber-400"> {formatNumber(result.errors)} could not be moved.</span>
        ) : null}
        {result.cancelled ? " Files already moved stay moved." : null}
      </p>
      <p className="selectable mt-1 break-all font-mono text-[11px] text-zinc-500">{result.trashDir}/</p>
      <p className="mt-1 text-[11px] text-zinc-500">
        You can now format the card or empty <span className="font-mono">{result.trashDir}</span> yourself.
      </p>
      {!result.cancelled ? (
        <div className="mt-2">
          <Button icon={ArrowUpTrayIcon} size="sm" variant="secondary" onClick={onEject} loading={ejecting}>
            Eject now
          </Button>
        </div>
      ) : null}
    </div>
  );
}
