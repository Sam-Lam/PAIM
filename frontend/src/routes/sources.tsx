import { useEffect, useMemo, useState } from "react";
import { Link, useNavigate } from "@tanstack/react-router";
import {
  ArrowDownTrayIcon,
  ArrowPathIcon,
  CheckBadgeIcon,
  ExclamationTriangleIcon,
  FingerPrintIcon,
  GlobeAltIcon,
  ServerStackIcon,
  ShieldCheckIcon,
} from "@heroicons/react/24/outline";
import {
  Button,
  Card,
  ClearSourceControl,
  DataTable,
  EmptyState,
  LoadingBlock,
  PageHeader,
  SafeToEraseBadge,
  StatusBadge,
  type DataTableColumn,
} from "../components";
import {
  SourcesService,
  WailsEvents,
  type ActiveSafeToEraseDTO,
  type MatchDTO,
  type SafeToEraseDTO,
  type SourceDTO,
  type SourceEvaluated,
  type SourceProgress,
  type VolumeDTO,
} from "../lib/api";
import { useAsyncData, usePoll, useWailsEvent } from "../lib/hooks";
import { useToast } from "../lib/toast";
import { formatBytes, formatNumber, formatRelative } from "../lib/format";

export function SourcesPage() {
  const toast = useToast();
  const volumes = useAsyncData(() => SourcesService.ListVolumes());
  const known = useAsyncData(() => SourcesService.ListKnownSources());

  // A single safe-to-erase evaluation runs at a time; its state is tracked here
  // and routed to the matching volume card by mountPoint. Re-attaching on mount
  // means navigating away and back never loses a running evaluation or its report.
  const [erase, setErase] = useState<ActiveSafeToEraseDTO | null>(null);
  // Live "Scanned N files…" counts for in-flight Identify calls, keyed by mount.
  const [identifyScan, setIdentifyScan] = useState<Record<string, number>>({});

  useEffect(() => {
    void SourcesService.ActiveSafeToErase()
      .then((dto) => setErase(dto.state === "none" ? null : dto))
      .catch(() => undefined);
  }, []);

  const refreshAll = () => {
    void volumes.run().catch((e) => toast.fromError(e, "Failed to list volumes"));
    void known.run({ silent: true }).catch(() => undefined);
  };

  usePoll(() => {
    void volumes.run({ silent: true }).catch(() => undefined);
    void known.run({ silent: true }).catch(() => undefined);
  }, 0); // initial load only; live updates come from events below.

  useWailsEvent(WailsEvents.VolumeMounted, () => {
    void volumes.run({ silent: true });
  });
  useWailsEvent(WailsEvents.VolumeUnmounted, () => {
    void volumes.run({ silent: true });
  });
  useWailsEvent(WailsEvents.SourceIdentified, () => {
    void known.run({ silent: true });
  });
  useWailsEvent<SourceProgress>(WailsEvents.SourceProgress, (p) => {
    if (p.kind === "safe-to-erase") {
      setErase({ state: "running", mountPoint: p.mountPoint, sourceId: "", progress: p, report: null, cancelled: false, error: "" });
    } else if (p.kind === "identify") {
      setIdentifyScan((m) => ({ ...m, [p.mountPoint]: p.scanned }));
    }
  });
  useWailsEvent<SourceEvaluated>(WailsEvents.SourceEvaluated, (e) => {
    setErase({
      state: "completed",
      mountPoint: e.mountPoint,
      sourceId: e.sourceId,
      progress: null,
      report: e.report,
      cancelled: e.cancelled,
      error: e.error,
    });
    void known.run({ silent: true });
  });

  return (
    <div>
      <PageHeader
        title="Sources"
        description="Connected volumes and previously identified import sources. Identify a volume to see why PAIM recognizes it, and whether it is safe to erase."
        actions={
          <Button icon={ArrowPathIcon} onClick={refreshAll} loading={volumes.loading && !!volumes.data}>
            Refresh
          </Button>
        }
      />

      <div className="space-y-6">
        <section>
          <h2 className="mb-3 text-xs font-semibold tracking-wide text-zinc-500 uppercase">Connected Volumes</h2>
          {volumes.loading && !volumes.data ? (
            <LoadingBlock label="Scanning volumes…" />
          ) : volumes.error && !volumes.data ? (
            <EmptyState
              icon={ExclamationTriangleIcon}
              title="Could not list volumes"
              description={volumes.error}
              action={<Button onClick={refreshAll}>Try again</Button>}
            />
          ) : !volumes.data || volumes.data.length === 0 ? (
            <Card>
              <EmptyState
                icon={ServerStackIcon}
                title="No volumes connected"
                description="Connect an SD card, USB SSD, or external drive to import from it."
              />
            </Card>
          ) : (
            <div className="grid gap-3 lg:grid-cols-2">
              {(volumes.data ?? []).map((v) => (
                <VolumeCard
                  key={v.mountPoint}
                  volume={v}
                  onIdentified={() => void known.run({ silent: true })}
                  erase={erase && erase.mountPoint === v.mountPoint ? erase : null}
                  identifyScanned={identifyScan[v.mountPoint]}
                />
              ))}
            </div>
          )}
        </section>

        <section>
          <h2 className="mb-3 text-xs font-semibold tracking-wide text-zinc-500 uppercase">Known Sources</h2>
          <Card flush>
            <KnownSourcesTable sources={known.data ?? []} loading={known.loading} />
          </Card>
        </section>
      </div>
    </div>
  );
}

function connectionIcon(v: VolumeDTO) {
  if (v.isNetworkVolume) return GlobeAltIcon;
  return ServerStackIcon;
}

function VolumeCard({
  volume,
  onIdentified,
  erase,
  identifyScanned,
}: {
  volume: VolumeDTO;
  onIdentified: () => void;
  erase: ActiveSafeToEraseDTO | null;
  identifyScanned?: number;
}) {
  const toast = useToast();
  const navigate = useNavigate();
  const [match, setMatch] = useState<MatchDTO | null>(null);
  const [identifying, setIdentifying] = useState(false);

  const Icon = connectionIcon(volume);
  const usedPct =
    volume.capacityBytes > 0
      ? Math.min(100, Math.round(((volume.capacityBytes - volume.freeBytes) / volume.capacityBytes) * 100))
      : 0;

  const evaluating = erase?.state === "running";

  const identify = async () => {
    setIdentifying(true);
    try {
      const result = await SourcesService.IdentifyVolume(volume.mountPoint);
      setMatch(result);
      onIdentified();
    } catch (e) {
      toast.fromError(e, "Identification failed");
    } finally {
      setIdentifying(false);
    }
  };

  const evaluate = async () => {
    try {
      await SourcesService.StartSafeToErase(match?.sourceId ?? "", volume.mountPoint);
    } catch (e) {
      toast.fromError(e, "Safe-to-erase evaluation failed");
    }
  };

  const cancelEvaluate = () => {
    void SourcesService.CancelSafeToErase().catch(() => undefined);
  };

  return (
    <Card>
      <div className="flex items-start gap-3">
        <div className="flex h-10 w-10 flex-none items-center justify-center rounded-lg bg-zinc-800 text-zinc-300">
          <Icon className="h-5 w-5" />
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <h3 className="truncate text-sm font-semibold text-zinc-100">
              {volume.volumeName || volume.mountPoint}
            </h3>
            {volume.internal ? <StatusBadge status="internal" tone="muted" label="Internal" /> : null}
            {volume.removable ? <StatusBadge status="removable" tone="info" label="Removable" /> : null}
          </div>
          <p className="selectable mt-0.5 truncate text-[11px] text-zinc-500" title={volume.mountPoint}>
            {volume.mountPoint}
          </p>

          <div className="mt-3 grid grid-cols-2 gap-x-4 gap-y-1 text-[11px] text-zinc-500">
            <Field label="Filesystem" value={volume.filesystemType || "—"} />
            <Field label="Connection" value={volume.connectionType || (volume.isNetworkVolume ? "Network" : "—")} />
            <Field label="Capacity" value={formatBytes(volume.capacityBytes)} />
            <Field label="Free" value={formatBytes(volume.freeBytes)} />
          </div>

          {volume.capacityBytes > 0 ? (
            <div className="mt-2">
              <div className="h-1.5 w-full overflow-hidden rounded-full bg-zinc-800">
                <div className="h-full rounded-full bg-zinc-500" style={{ width: `${usedPct}%` }} />
              </div>
              <p className="mt-1 text-[10px] text-zinc-600">{usedPct}% used</p>
            </div>
          ) : null}

          {volume.warnings && volume.warnings.length > 0 ? (
            <ul className="mt-2 space-y-0.5">
              {(volume.warnings ?? []).map((w, i) => (
                <li key={i} className="flex items-center gap-1 text-[11px] text-amber-400/90">
                  <ExclamationTriangleIcon className="h-3.5 w-3.5 flex-none" />
                  {w}
                </li>
              ))}
            </ul>
          ) : null}
        </div>
      </div>

      {/* Actions */}
      <div className="mt-4 flex flex-wrap items-center gap-2">
        <Button icon={FingerPrintIcon} onClick={identify} loading={identifying}>
          Identify
        </Button>
        {identifying ? (
          <span className="flex items-center gap-2 text-[11px] text-zinc-500">
            <span className="tabular-nums">Scanned {formatNumber(identifyScanned ?? 0)} files…</span>
            <button className="text-blue-400 hover:underline" onClick={() => void SourcesService.CancelIdentify()}>
              Cancel
            </button>
          </span>
        ) : null}
        <Button icon={ShieldCheckIcon} onClick={evaluate} loading={evaluating} disabled={evaluating} variant="secondary">
          Evaluate Safe to Erase
        </Button>
        <Button
          icon={ArrowDownTrayIcon}
          variant="primary"
          onClick={() => navigate({ to: "/import", search: { root: volume.mountPoint } })}
        >
          Import from this volume
        </Button>
      </div>

      {match ? <MatchPanel match={match} /> : null}
      {erase?.state === "running" ? (
        <SafeToEraseProgress progress={erase.progress} onCancel={cancelEvaluate} />
      ) : null}
      {erase?.state === "completed" && erase.report ? (
        <SafeToErasePanel report={erase.report} root={volume.mountPoint} />
      ) : null}
      {erase?.state === "completed" && erase.cancelled ? (
        <p className="mt-3 text-[11px] text-zinc-500">Safe-to-erase evaluation cancelled.</p>
      ) : null}
      {erase?.state === "completed" && erase.error ? (
        <p className="mt-3 text-[11px] text-red-400">Evaluation failed: {erase.error}</p>
      ) : null}
    </Card>
  );
}

// SafeToEraseProgress renders the determinate progress panel on a volume card
// while a background safe-to-erase evaluation is hashing its files.
function SafeToEraseProgress({ progress, onCancel }: { progress: SourceProgress | null; onCancel: () => void }) {
  const done = progress?.filesDone ?? 0;
  const total = progress?.filesTotal ?? 0;
  const pct = total > 0 ? Math.min(100, Math.round((done / total) * 100)) : 0;
  return (
    <div className="mt-3 rounded-lg border border-zinc-800 bg-zinc-950/40 p-3">
      <div className="mb-2 flex items-center justify-between">
        <h4 className="text-xs font-semibold text-zinc-300">Evaluating safe to erase…</h4>
        <button className="text-[11px] text-blue-400 hover:underline" onClick={onCancel}>
          Cancel
        </button>
      </div>
      <div className="h-2 w-full overflow-hidden rounded-full bg-zinc-800">
        <div className="h-full rounded-full bg-blue-500 transition-all" style={{ width: `${pct}%` }} />
      </div>
      <div className="mt-1 flex items-center justify-between text-[10px] text-zinc-500">
        <span className="tabular-nums">
          {formatNumber(done)} of {formatNumber(total)} files hashed
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

function confidenceTone(confidence: number): { bar: string; text: string } {
  if (confidence >= 90) return { bar: "bg-emerald-500", text: "text-emerald-400" };
  if (confidence >= 60) return { bar: "bg-blue-500", text: "text-blue-400" };
  if (confidence >= 30) return { bar: "bg-amber-500", text: "text-amber-400" };
  return { bar: "bg-zinc-500", text: "text-zinc-400" };
}

function MatchPanel({ match }: { match: MatchDTO }) {
  const tone = confidenceTone(match.confidence);
  return (
    <div className="mt-4 rounded-lg border border-zinc-800 bg-zinc-950/40 p-3">
      <div className="mb-2 flex items-center justify-between">
        <h4 className="text-xs font-semibold text-zinc-300">Identification</h4>
        <div className="flex items-center gap-2">
          {match.isKnown ? <StatusBadge status="known" tone="success" label="Known device" dot /> : (
            <StatusBadge status="new" tone="neutral" label="New device" dot />
          )}
          {match.contentsPreviouslyImported ? (
            <StatusBadge status="imported" tone="info" label="Contents previously imported" />
          ) : null}
        </div>
      </div>

      {/* Confidence meter */}
      <div className="mb-3">
        <div className="mb-1 flex items-center justify-between text-[11px]">
          <span className="text-zinc-500">Confidence</span>
          <span className={`font-semibold tabular-nums ${tone.text}`}>{match.confidence}/100</span>
        </div>
        <div className="h-2 w-full overflow-hidden rounded-full bg-zinc-800">
          <div className={`h-full rounded-full ${tone.bar}`} style={{ width: `${Math.max(0, Math.min(100, match.confidence))}%` }} />
        </div>
      </div>

      {/* The WHY — rendered prominently. */}
      {match.reasons && match.reasons.length > 0 ? (
        <div>
          <p className="mb-1 text-[11px] font-medium text-zinc-500">Why</p>
          <ul className="space-y-1">
            {(match.reasons ?? []).map((r, i) => (
              <li key={i} className="flex items-start gap-2 text-[12px] text-zinc-300">
                <CheckBadgeIcon className="mt-0.5 h-3.5 w-3.5 flex-none text-blue-400" />
                <span>{r}</span>
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      <div className="mt-3 flex items-center gap-4 border-t border-zinc-800 pt-2 text-[11px] text-zinc-500">
        <span>{formatNumber(match.source.importCount)} imports</span>
        <span>Last seen {formatRelative(match.source.lastSeenAt)}</span>
      </div>
    </div>
  );
}

function SafeToErasePanel({ report, root }: { report: SafeToEraseDTO; root: string }) {
  return (
    <div className="mt-3 rounded-lg border border-zinc-800 bg-zinc-950/40 p-3">
      <div className="mb-2 flex items-center justify-between">
        <h4 className="text-xs font-semibold text-zinc-300">Safe to Erase</h4>
        <SafeToEraseBadge safe={report.safe} noBackupDestination={report.noBackupDestination} />
      </div>
      <p
        className={`text-[12px] leading-relaxed ${
          report.noBackupDestination ? "text-amber-400/90" : "text-zinc-400"
        }`}
      >
        {report.reason}
      </p>
      {report.noBackupDestination ? (
        <Link
          to="/providers"
          className="mt-1 inline-block text-[11px] font-semibold text-amber-300 hover:text-amber-200"
        >
          Add a backup destination →
        </Link>
      ) : null}
      <div className="mt-3 grid grid-cols-2 gap-2 sm:grid-cols-5">
        <MiniStat label="Total media" value={report.totalMedia} />
        <MiniStat label="Fully protected" value={report.archived} tone="success" />
        <MiniStat label="New" value={report.new} tone={report.new > 0 ? "warn" : "default"} />
        <MiniStat label="Unverified" value={report.unverified} tone={report.unverified > 0 ? "warn" : "default"} />
        {/* Archived + verified but backups still pending — amber, not red:
            nothing here is at risk, the backups simply have not finished. */}
        <MiniStat label="Archived — backup pending" value={report.backupIncomplete} tone={report.backupIncomplete > 0 ? "warn" : "default"} />
      </div>
      {report.fastPath + report.hashed > 0 ? (
        <p className="mt-2 text-[10px] text-zinc-600">
          {formatNumber(report.fastPath)} verified from the catalog without re-reading · {formatNumber(report.hashed)} re-hashed
        </p>
      ) : null}
      {report.backupIncomplete > 0 && report.new === 0 && report.unverified === 0 && !report.noBackupDestination ? (
        <p className="mt-2 text-[11px] text-zinc-500">
          Backups run in the background — track them in the{" "}
          <Link to="/backup-queue" className="text-blue-400 hover:text-blue-300">
            Backup Queue
          </Link>
          .
        </p>
      ) : null}
      {report.safe ? <ClearSourceControl root={root} report={report} fresh /> : null}
    </div>
  );
}

function MiniStat({
  label,
  value,
  tone = "default",
}: {
  label: string;
  value: number;
  tone?: "default" | "success" | "warn" | "danger";
}) {
  const color =
    tone === "success"
      ? "text-emerald-400"
      : tone === "warn"
        ? "text-amber-400"
        : tone === "danger"
          ? "text-red-400"
          : "text-zinc-200";
  return (
    <div className="rounded-md border border-zinc-800 bg-zinc-900/60 px-2 py-1.5 text-center">
      <div className={`text-sm font-semibold tabular-nums ${color}`}>{formatNumber(value)}</div>
      <div className="mt-0.5 text-[10px] text-zinc-500">{label}</div>
    </div>
  );
}

function KnownSourcesTable({ sources, loading }: { sources: SourceDTO[]; loading: boolean }) {
  const columns = useMemo<DataTableColumn<SourceDTO>[]>(
    () => [
      {
        id: "label",
        header: "Label",
        accessorFn: (s) => s.volumeLabel || s.model || s.hardwareSerial || "Unknown",
        cell: ({ row }) => {
          const s = row.original;
          return (
            <div className="min-w-0">
              <div className="truncate font-medium text-zinc-200">
                {s.volumeLabel || s.model || s.hardwareSerial || "Unknown"}
              </div>
              {s.manufacturer ? <div className="truncate text-[11px] text-zinc-500">{s.manufacturer}</div> : null}
            </div>
          );
        },
      },
      {
        id: "type",
        header: "Type",
        accessorKey: "sourceType",
        cell: ({ row }) => <StatusBadge status={row.original.sourceType} tone="muted" />,
      },
      {
        id: "confidence",
        header: "Confidence",
        accessorKey: "confidence",
        cell: ({ row }) => {
          const tone = confidenceTone(row.original.confidence);
          return <span className={`tabular-nums font-medium ${tone.text}`}>{row.original.confidence}</span>;
        },
      },
      {
        id: "lastSeen",
        header: "Last Seen",
        accessorKey: "lastSeenAt",
        cell: ({ row }) => <span className="text-zinc-400">{formatRelative(row.original.lastSeenAt)}</span>,
      },
      {
        id: "imports",
        header: "Imports",
        accessorKey: "importCount",
        cell: ({ row }) => <span className="tabular-nums text-zinc-400">{formatNumber(row.original.importCount)}</span>,
      },
      {
        id: "safe",
        header: "Safe to Erase",
        accessorFn: (s) => (s.safeToErase ? 1 : 0),
        cell: ({ row }) => <SafeToEraseBadge safe={row.original.safeToErase} />,
      },
    ],
    [],
  );

  return (
    <DataTable
      data={sources}
      columns={columns}
      loading={loading}
      getRowId={(s) => s.id}
      initialSorting={[{ id: "lastSeen", desc: true }]}
      emptyState={{
        icon: ServerStackIcon,
        title: "No known sources",
        description: "Identify a connected volume to remember it here.",
      }}
    />
  );
}
