import { useCallback, useState } from "react";
import { ConfirmDialog } from "./ConfirmDialog";
import { useWailsEvent } from "../lib/hooks";
import { AppService, WailsEvents, type OperationInfo, type QuitRequested } from "../lib/api";
import { formatBytes, formatNumber } from "../lib/format";

// Human sentence for one running operation, with live numbers. The backend
// supplies a machine `kind` and a fallback `label`; we phrase per kind and append
// the file or byte progress when known (a zero total means indeterminate).
function describeOperation(op: OperationInfo): string {
  const lead =
    {
      import: "An import is running",
      analyze: "A source analysis is running",
      reorganize: "A library reorganization is running",
      safe_to_erase: "A safe-to-erase check is running",
      cleanup: "A folder cleanup analysis is running",
      backup_upload: "A backup upload is running",
    }[op.kind] ?? op.label;

  if (op.kind === "backup_upload") {
    if (op.bytesTotal > 0) {
      return `${lead}: ${formatBytes(op.bytesDone)} of ${formatBytes(op.bytesTotal)}`;
    }
    return lead;
  }

  if (op.filesTotal > 0) {
    return `${lead}: ${formatNumber(op.filesDone)} of ${formatNumber(op.filesTotal)} files`;
  }
  if (op.filesDone > 0) {
    return `${lead}: ${formatNumber(op.filesDone)} files so far`;
  }
  return lead;
}

/**
 * Global quit guard. When the user tries to quit (Cmd+Q, menu, or last-window
 * close) while long operations are running, the Go ShouldQuit hook vetoes the
 * quit and emits app:quit-requested with the live operations. This dialog names
 * each one and offers "Keep running" (dismiss) or "Quit anyway" (cancel all
 * operations, let the current file settle, then quit). A repeated quit attempt
 * re-emits with fresh numbers, which simply re-renders this dialog.
 *
 * Rendered once in the root layout so it appears over any page.
 */
export function QuitGuardDialog() {
  const [ops, setOps] = useState<OperationInfo[] | null>(null);
  const [quitting, setQuitting] = useState(false);

  useWailsEvent<QuitRequested>(WailsEvents.QuitRequested, (data) => {
    // Replace with the latest snapshot so a repeat Cmd+Q shows fresh numbers.
    setOps(data?.operations ?? []);
  });

  const onCancel = useCallback(() => {
    setOps(null);
    setQuitting(false);
  }, []);

  const onConfirm = useCallback(async () => {
    setQuitting(true);
    try {
      await AppService.ConfirmQuit();
      // On success the app quits and this view is torn down; nothing more to do.
    } catch {
      // If the quit could not be triggered, drop the loading state so the user
      // can retry or keep running.
      setQuitting(false);
    }
  }, []);

  const open = ops !== null && ops.length > 0;
  if (!open) return null;

  const description = (
    <div>
      <p>The following {ops.length === 1 ? "operation is" : "operations are"} still running:</p>
      <ul className="mt-2 list-disc space-y-1 pl-5">
        {ops.map((op, i) => (
          <li key={`${op.kind}-${i}`}>{describeOperation(op)}</li>
        ))}
      </ul>
      <p className="mt-3">
        Quitting is safe: work pauses cleanly and resumes next time you open PAIM. Nothing is
        lost.
      </p>
    </div>
  );

  return (
    <ConfirmDialog
      open
      title="Quit while work is in progress?"
      description={description}
      confirmLabel="Quit anyway"
      cancelLabel="Keep running"
      variant="danger"
      loading={quitting}
      loadingContent={<p className="text-[13px] text-zinc-400">Finishing the current file…</p>}
      onConfirm={onConfirm}
      onCancel={onCancel}
    />
  );
}
