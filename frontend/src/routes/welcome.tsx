import { useEffect, useState } from "react";
import { ClockIcon, FolderOpenIcon, FolderPlusIcon, ArrowUpTrayIcon } from "@heroicons/react/24/outline";
import { Button, Card, ConfirmDialog, Spinner } from "../components";
import {
  LibraryService,
  WailsEvents,
  type LegacyStatusDTO,
  type LibraryProgress,
  type LockConflictDTO,
  type OpenResultDTO,
  type RecentLibraryDTO,
} from "../lib/api";
import { useLibrary } from "../lib/library";
import { useWailsEvent } from "../lib/hooks";
import { useToast } from "../lib/toast";

const APP_VERSION = "0.1.0";

/** A pending lock conflict: the library the user tried to open plus its details. */
interface Conflict {
  path: string;
  detail: LockConflictDTO;
}

/**
 * Welcome — shown whenever no library is open. Offers creating a new library,
 * opening an existing one (or a recent), migrating the legacy per-machine
 * catalog, and resolving lock conflicts via an explicit Force Open.
 */
export function WelcomePage() {
  const { refresh } = useLibrary();
  const toast = useToast();

  const [recent, setRecent] = useState<RecentLibraryDTO[]>([]);
  const [legacy, setLegacy] = useState<LegacyStatusDTO | null>(null);
  const [busy, setBusy] = useState(false);
  const [conflict, setConflict] = useState<Conflict | null>(null);
  const [forcing, setForcing] = useState(false);
  const [relaunch, setRelaunch] = useState(false);
  // Live migration/open phase, so a legacy migration or catalog upgrade shows
  // labeled progress and a prominent "don't quit" warning instead of a bare spinner.
  const [phase, setPhase] = useState<LibraryProgress | null>(null);

  useWailsEvent<LibraryProgress>(WailsEvents.LibraryProgress, (p) => {
    setPhase(p.phase === "done" ? null : p);
  });

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const [r, l] = await Promise.all([LibraryService.Recent(), LibraryService.LegacyStatus()]);
        if (cancelled) return;
        setRecent(r ?? []);
        setLegacy(l);
      } catch (e) {
        if (!cancelled) toast.fromError(e, "Could not load libraries");
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // handleResult maps an OpenResult to UI: proceed, prompt relaunch, or surface a
  // lock conflict for a Force Open decision.
  const handleResult = async (res: OpenResultDTO, path: string): Promise<boolean> => {
    if (res.lockConflict) {
      setConflict({ path, detail: res.lockConflict });
      return false;
    }
    if (res.needsRelaunch) {
      setRelaunch(true);
      return false;
    }
    if (res.library) {
      await refresh(); // the root layout leaves Welcome once current is set
      return true;
    }
    return false;
  };

  const run = async (fn: () => Promise<OpenResultDTO>, path: string, failMsg: string) => {
    setBusy(true);
    try {
      const res = await fn();
      await handleResult(res, path);
    } catch (e) {
      toast.fromError(e, failMsg);
    } finally {
      setBusy(false);
      setPhase(null);
    }
  };

  // A migrating/legacy-install phase must not be interrupted — the catalog is
  // being rewritten. Surface it prominently.
  const migrationInFlight = phase?.phase === "migrating" || phase?.phase === "installing-legacy";

  const createLibrary = async () => {
    const path = await pickFolder(toast);
    if (!path) return;
    await run(() => LibraryService.Create(path), path, "Could not create library");
  };

  const openLibrary = async () => {
    const path = await pickFolder(toast);
    if (!path) return;
    await run(() => LibraryService.Open(path), path, "Could not open library");
  };

  const openRecent = async (path: string) => {
    await run(() => LibraryService.Open(path), path, "Could not open library");
  };

  const migrateLegacy = async () => {
    const path = await pickFolder(toast);
    if (!path) return;
    await run(() => LibraryService.MigrateLegacy(path), path, "Could not migrate legacy catalog");
  };

  const doForceOpen = async () => {
    if (!conflict) return;
    setForcing(true);
    try {
      const res = await LibraryService.ForceOpen(conflict.path);
      const done = await handleResult(res, conflict.path);
      if (done) setConflict(null);
    } catch (e) {
      toast.fromError(e, "Force Open failed");
    } finally {
      setForcing(false);
    }
  };

  return (
    <div className="mx-auto flex min-h-screen max-w-2xl flex-col justify-center px-6 py-12">
      <div className="mb-8 flex items-center gap-3" style={{ "--wails-draggable": "no-drag" } as React.CSSProperties}>
        <div className="flex h-11 w-11 flex-none items-center justify-center rounded-xl bg-blue-600 text-base font-bold text-white">
          PA
        </div>
        <div>
          <h1 className="text-lg font-semibold text-zinc-100">Photo Archive Integrity Manager</h1>
          <p className="text-[13px] text-zinc-500">Choose a library to begin. Version {APP_VERSION}.</p>
        </div>
      </div>

      <div className="space-y-4" style={{ "--wails-draggable": "no-drag" } as React.CSSProperties}>
        <Card
          title="Open a library"
          subtitle="A library is a folder whose catalog lives inside it — it travels with your photos."
        >
          <div className="flex flex-wrap gap-3">
            <Button icon={FolderPlusIcon} variant="primary" onClick={() => void createLibrary()} disabled={busy}>
              Create new library…
            </Button>
            <Button icon={FolderOpenIcon} variant="secondary" onClick={() => void openLibrary()} disabled={busy}>
              Open existing…
            </Button>
            {busy && !phase ? <Spinner /> : null}
            {busy && phase ? (
              <span className="flex items-center gap-2 text-[12px] text-zinc-400">
                <Spinner />
                {phase.message || phaseLabel(phase.phase)}
              </span>
            ) : null}
          </div>

          {migrationInFlight ? (
            <div className="mt-3 flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-[12px] font-medium text-amber-200">
              <ArrowUpTrayIcon className="mt-0.5 h-4 w-4 flex-none" />
              <span>
                Upgrading library catalog — don't quit PAIM. This backs up and converts your catalog and must finish
                before the library opens.
              </span>
            </div>
          ) : null}
        </Card>

        {recent.length > 0 ? (
          <Card title="Recent libraries">
            <ul className="divide-y divide-zinc-800">
              {recent.map((r) => (
                <li key={r.path} className="flex items-center justify-between gap-3 py-2.5">
                  <div className="min-w-0">
                    <div className="truncate text-[13px] font-medium text-zinc-200">{r.name}</div>
                    <div className="truncate font-mono text-[11px] text-zinc-500">{r.path}</div>
                  </div>
                  <Button
                    size="sm"
                    variant="ghost"
                    icon={ClockIcon}
                    onClick={() => void openRecent(r.path)}
                    disabled={busy}
                  >
                    Open
                  </Button>
                </li>
              ))}
            </ul>
          </Card>
        ) : null}

        {legacy?.exists ? (
          <Card
            title="Migrate your existing catalog"
            subtitle="A pre-library PAIM catalog was found on this Mac. Move it into a library folder to make it portable."
          >
            <p className="mb-3 font-mono text-[11px] text-zinc-500">{legacy.path}</p>
            <Button icon={ArrowUpTrayIcon} variant="secondary" onClick={() => void migrateLegacy()} disabled={busy}>
              Choose folder & migrate…
            </Button>
            <p className="mt-2 text-[11px] text-zinc-500">
              Your original catalog is copied (and kept as a backup) — nothing is deleted.
            </p>
          </Card>
        ) : null}
      </div>

      <ConfirmDialog
        open={conflict !== null}
        title="Library is locked"
        variant="danger"
        confirmLabel="Force Open"
        requireWord="FORCE"
        loading={forcing}
        description={
          conflict ? (
            <div className="space-y-2">
              <p>{conflict.detail.message}</p>
              <div className="rounded-md border border-zinc-800 bg-zinc-950 p-2.5 text-[12px]">
                <div>
                  Host: <span className="font-mono text-zinc-300">{conflict.detail.hostname || "unknown"}</span>
                </div>
                <div>
                  PID: <span className="font-mono text-zinc-300">{conflict.detail.pid}</span>
                </div>
                <div>
                  Last active: <span className="font-mono text-zinc-300">{conflict.detail.heartbeatAgeSeconds}s ago</span>
                </div>
              </div>
              {conflict.detail.sameHostLive ? (
                <p className="text-amber-300/80">
                  This library appears to be open in another PAIM window on this Mac. Forcing open risks two writers —
                  only continue if you are sure the other instance is gone.
                </p>
              ) : (
                <p className="text-zinc-400">
                  Only Force Open if that machine has crashed or unmounted the drive. Two machines writing at once will
                  corrupt the catalog.
                </p>
              )}
            </div>
          ) : null
        }
        onConfirm={() => void doForceOpen()}
        onCancel={() => setConflict(null)}
      />

      <ConfirmDialog
        open={relaunch}
        title="Relaunch required"
        variant="primary"
        confirmLabel="Got it"
        cancelLabel="Dismiss"
        description="Your choice was saved. Quit and reopen PAIM to switch to the selected library."
        onConfirm={() => setRelaunch(false)}
        onCancel={() => setRelaunch(false)}
      />
    </div>
  );
}

// pickFolder opens the native library-folder chooser, surfacing dialog errors.
async function pickFolder(toast: ReturnType<typeof useToast>): Promise<string> {
  try {
    return (await LibraryService.PickLibraryFolder()) ?? "";
  } catch (e) {
    toast.fromError(e, "Could not open folder picker");
    return "";
  }
}

// phaseLabel maps a library:progress phase to human text (fallback when the
// event carries no message).
function phaseLabel(phase: string): string {
  switch (phase) {
    case "installing-legacy":
      return "Copying and verifying legacy catalog…";
    case "migrating":
      return "Upgrading library catalog…";
    case "backing-up":
      return "Backing up catalog…";
    case "converting-paths":
      return "Converting archive paths…";
    case "verifying":
      return "Verifying…";
    default:
      return "Working…";
  }
}
