import { useRef } from "react";
import { useNavigate } from "@tanstack/react-router";
import { useWailsEvent } from "../lib/hooks";
import { useToast } from "../lib/toast";
import {
  AppService,
  SourcesService,
  WailsEvents,
  type BackupBackfillCompleted,
  type BackupProviderFailing,
  type SourceCleared,
  type VolumeEvent,
} from "../lib/api";

// Foreground operation kinds (mirror internal/services/activity.go). While one is
// running we suppress the card-arrival "Import?" toast so it never interrupts an
// active import/analyze/reorganize/safe-to-erase/cleanup/clear workflow.
const FOREGROUND_KINDS = new Set([
  "import",
  "analyze",
  "reorganize",
  "safe_to_erase",
  "cleanup",
  "clear_source",
]);

// De-dupe window for repeated mount events on the same volume (a single insert
// can reconcile more than once, and macOS can remount).
const MOUNT_DEDUPE_MS = 60_000;

// rclone's expired-credential message; when a provider fails with it we add the
// exact reconnect hint.
const RECONNECT_RE = /rclone config reconnect (\S+)/;

/**
 * GlobalNotifications is the single app-wide bridge from low-frequency backend
 * events to toasts. It is mounted once in the root layout (a sibling of the quit
 * guard) so it overlays any page. It surfaces exactly three signals:
 *  - a removable card/drive mounting → offer to import from it;
 *  - a provider backfill completing → confirm the queued backups;
 *  - a provider entering a failing state → point at the Backup Queue.
 */
export function GlobalNotifications() {
  const toast = useToast();
  const navigate = useNavigate();
  const lastMount = useRef<Map<string, number>>(new Map());

  // Card arrival: a removable, ejectable, non-library volume mounted. Offer to
  // import from it — but not while a foreground operation is running, and not
  // twice for the same mount within a minute.
  useWailsEvent<VolumeEvent>(WailsEvents.VolumeMounted, (ev) => {
    if (!ev || !ev.mountPoint) return;
    if (!(ev.removable || ev.ejectable) || ev.isLibraryVolume) return;

    const now = Date.now();
    const prev = lastMount.current.get(ev.mountPoint);
    if (prev != null && now - prev < MOUNT_DEDUPE_MS) return;
    lastMount.current.set(ev.mountPoint, now);

    void (async () => {
      try {
        const ops = (await AppService.ActiveOperations()) ?? [];
        if (ops.some((op) => op != null && FOREGROUND_KINDS.has(op.kind))) return;
      } catch {
        // If we cannot read activity, err on the side of offering the import.
      }
      const name = ev.volumeName?.trim() || "Removable volume";
      toast.info(`Volume "${name}" mounted`, "Import photos and videos from it?", {
        label: "Import…",
        onClick: () => void navigate({ to: "/import", search: { root: ev.mountPoint } }),
      });
    })();
  });

  // Source clear completed: if the cleared source's removable volume is still
  // mounted, gently offer to eject it — once per clear (no nagging loop).
  useWailsEvent<SourceCleared>(WailsEvents.SourceCleared, (ev) => {
    if (!ev || ev.cancelled || !ev.root) return;
    void (async () => {
      try {
        const vols = (await SourcesService.ListVolumes()) ?? [];
        const onVol = vols.find(
          (v) => v != null && v.mountPoint && (ev.root === v.mountPoint || ev.root.startsWith(v.mountPoint + "/")),
        );
        if (!onVol || !(onVol.removable || onVol.ejectable)) return;
        const name = onVol.volumeName?.trim() || "the source";
        toast.info(`Source cleared — eject ${name}?`, "It is safe to unplug once ejected.", {
          label: "Eject",
          onClick: () => {
            void SourcesService.EjectVolume(ev.root)
              .then(() => toast.success("Ejected", "Safe to unplug."))
              .catch((e) => toast.fromError(e, "Could not eject"));
          },
        });
      } catch {
        // Best-effort reminder — if volumes cannot be listed, simply do nothing.
      }
    })();
  });

  // Provider backfill completed: confirm how many backups were queued.
  useWailsEvent<BackupBackfillCompleted>(WailsEvents.BackupBackfillCompleted, (ev) => {
    if (!ev || ev.cancelled || (ev.enqueued ?? 0) <= 0) return;
    const where = ev.providerId ? ` for ${ev.providerId}` : "";
    toast.success(
      `Queued ${ev.enqueued} backup${ev.enqueued === 1 ? "" : "s"}${where}`,
      "They will upload in the background.",
    );
  });

  // Provider entered a failing state (throttled server-side to at most 1/hour per
  // provider). Point the user at the Backup Queue; add the rclone reconnect hint
  // when the failure is an expired-credential error.
  useWailsEvent<BackupProviderFailing>(WailsEvents.BackupProviderFailing, (ev) => {
    if (!ev) return;
    const name = ev.providerName?.trim() || ev.providerId || "a destination";
    const match = ev.providerName ? null : RECONNECT_RE.exec(ev.providerId ?? "");
    const hint = match ? `Run: rclone config reconnect ${match[1]}` : undefined;
    toast.warn(`Backups to ${name} are failing`, hint ?? "See the Backup Queue for details.", {
      label: "Open Backup Queue",
      onClick: () => void navigate({ to: "/backup-queue" }),
    });
  });

  return null;
}
