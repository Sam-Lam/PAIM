import { useEffect, useMemo, useState } from "react";
import {
  ArrowPathIcon,
  CheckCircleIcon,
  CircleStackIcon,
  CloudArrowUpIcon,
  ExclamationTriangleIcon,
  FolderOpenIcon,
  InformationCircleIcon,
  PlusIcon,
  ServerStackIcon,
  TrashIcon,
} from "@heroicons/react/24/outline";
import { Button, Card, EmptyState, LoadingBlock, PageHeader, ProgressBar, StatusBadge } from "../components";
import {
  BackupService,
  CleanupService,
  ProviderService,
  WailsEvents,
  type BackupBackfillCompleted,
  type BackupBackfillProgress,
  type PluginDTO,
  type ProviderDTO,
  type RcloneRemoteInfoDTO,
  type RcloneRemotesDTO,
} from "../lib/api";
import { useAsyncData, useWailsEvent } from "../lib/hooks";
import { useToast } from "../lib/toast";
import { formatBytes, formatNumber } from "../lib/format";

const ORDER_OPTIONS: { value: string; label: string }[] = [
  { value: "oldest_first", label: "Oldest first (FIFO)" },
  { value: "newest_first", label: "Newest first" },
];

/** Providers — configure backup destinations (localfs and future plugins). */
export function ProvidersPage() {
  const toast = useToast();
  const providers = useAsyncData(() => ProviderService.List());
  const plugins = useAsyncData(() => ProviderService.AvailablePlugins());
  const [adding, setAdding] = useState(false);
  // The single in-flight backfill (only one runs at a time), keyed by providerId.
  const [backfill, setBackfill] = useState<BackupBackfillProgress | null>(null);

  const refresh = () => providers.run({ silent: true });

  useEffect(() => {
    void providers.run().catch((e) => toast.fromError(e, "Failed to load providers"));
    void plugins.run().catch(() => undefined);
    // Re-attach to a backfill already running (e.g. after a route change).
    void BackupService.BackfillStatus()
      .then((st) => {
        if (st.running) setBackfill({ providerId: st.providerId, done: st.done, total: st.total, running: true });
      })
      .catch(() => undefined);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useWailsEvent<BackupBackfillProgress>(WailsEvents.BackupBackfillProgress, (d) => {
    setBackfill(d.running ? d : null);
  });
  useWailsEvent<BackupBackfillCompleted>(WailsEvents.BackupBackfillCompleted, (d) => {
    setBackfill(null);
    if (!d.cancelled) {
      const extra = d.skipped > 0 ? ` (${formatNumber(d.skipped)} already queued)` : "";
      toast.success(`Queued ${formatNumber(d.enqueued)} backup${d.enqueued === 1 ? "" : "s"}${extra}`);
    }
    void refresh();
  });

  const startBackfill = (providerId: string) =>
    void (async () => {
      try {
        const st = await BackupService.StartBackfill(providerId);
        setBackfill({ providerId, done: st.done, total: st.total, running: true });
      } catch (e) {
        toast.fromError(e, "Could not queue backups");
      }
    })();

  const pluginByName = useMemo(() => {
    const m = new Map<string, PluginDTO>();
    (plugins.data ?? []).forEach((p) => m.set(p.name, p));
    return m;
  }, [plugins.data]);

  return (
    <div>
      <PageHeader
        title="Providers"
        description="Backup destinations for your archive. Backups run automatically after each import — configure where they go here."
        actions={
          <Button
            icon={ArrowPathIcon}
            onClick={() => void providers.run().catch((e) => toast.fromError(e, "Failed to load providers"))}
            loading={providers.loading && !!providers.data}
          >
            Refresh
          </Button>
        }
      />

      <div className="mb-5 flex items-start gap-2 rounded-md border border-blue-500/30 bg-blue-500/5 p-3 text-[12px] text-blue-200/90">
        <InformationCircleIcon className="mt-0.5 h-4 w-4 flex-none" />
        <span>
          Every imported asset is automatically enqueued for backup to your enabled destinations. Required destinations
          gate safety verdicts (safe-to-erase, cleanup); <span className="font-medium">mirror</span> destinations are
          quality-of-life extras that never block those verdicts.
        </span>
      </div>

      {providers.loading && !providers.data ? (
        <LoadingBlock label="Loading destinations…" />
      ) : (
        <div className="space-y-3">
          {(providers.data ?? []).length === 0 ? (
            <Card>
              <EmptyState
                icon={ServerStackIcon}
                title="No backup destinations"
                description="Add a destination below so PAIM can back up your archived photos."
              />
            </Card>
          ) : (
            (providers.data ?? []).map((p) => (
              <ProviderCard
                key={p.id}
                provider={p}
                plugin={pluginByName.get(p.pluginName)}
                onChanged={refresh}
                backfill={backfill?.providerId === p.id ? backfill : null}
                backfillBusy={!!backfill}
                onQueueBackfill={() => startBackfill(p.id)}
              />
            ))
          )}

          {adding ? (
            <AddDestination
              plugins={plugins.data ?? []}
              onCancel={() => setAdding(false)}
              onAdded={() => {
                setAdding(false);
                void refresh();
              }}
            />
          ) : (
            <Button icon={PlusIcon} variant="secondary" onClick={() => setAdding(true)}>
              Add destination
            </Button>
          )}
        </div>
      )}
    </div>
  );
}

function ProviderCard({
  provider,
  plugin,
  onChanged,
  backfill,
  backfillBusy,
  onQueueBackfill,
}: {
  provider: ProviderDTO;
  plugin: PluginDTO | undefined;
  onChanged: () => void;
  backfill: BackupBackfillProgress | null;
  backfillBusy: boolean;
  onQueueBackfill: () => void;
}) {
  const toast = useToast();
  const [saving, setSaving] = useState(false);
  const summary = configSummary(provider.configJson);
  const missing = provider.missingBackupCount ?? 0;

  // Health: the destination is "failing" when its most recent outcome is a failure
  // (a currently-failed job newer than any success). It stays functionally enabled;
  // only the badge styling changes (amber, replacing the green Enabled dot).
  const lastError = provider.lastError ?? null;
  const lastSuccessAt = provider.lastSuccessAt ?? null;
  const isFailing =
    provider.enabled &&
    !!lastError &&
    (!lastSuccessAt || new Date(lastError.at).getTime() > new Date(lastSuccessAt).getTime());
  // rclone surfaces expired credentials with a reconnect command; surface the hint.
  const reconnectRemote = lastError ? /rclone config reconnect (\S+)/.exec(lastError.message)?.[1] : undefined;

  const save = async (patch: { enabled?: boolean; uploadOrder?: string }) => {
    setSaving(true);
    try {
      await ProviderService.Update(
        provider.id,
        provider.configJson,
        patch.enabled ?? provider.enabled,
        provider.mirror,
        patch.uploadOrder ?? provider.uploadOrder,
      );
      onChanged();
    } catch (e) {
      toast.fromError(e, "Could not update destination");
    } finally {
      setSaving(false);
    }
  };

  return (
    <Card>
      <div className="flex items-start gap-3">
        <div className="flex h-10 w-10 flex-none items-center justify-center rounded-lg bg-zinc-800 text-zinc-300">
          <CircleStackIcon className="h-5 w-5" />
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <h3 className="text-sm font-semibold text-zinc-100">{provider.pluginName}</h3>
            {provider.enabled && isFailing ? (
              <span title={lastError?.message}>
                <StatusBadge status="failing" tone="warn" label="Failing" dot />
              </span>
            ) : (
              <StatusBadge
                status={provider.enabled ? "enabled" : "disabled"}
                tone={provider.enabled ? "success" : "muted"}
                label={provider.enabled ? "Enabled" : "Disabled"}
                dot
              />
            )}
            {provider.mirror ? (
              <span className="rounded-full bg-amber-500/15 px-2 py-0.5 text-[10px] font-semibold tracking-wide text-amber-300 uppercase ring-1 ring-amber-500/30 ring-inset">
                Mirror
              </span>
            ) : null}
          </div>
          <p className="selectable mt-0.5 truncate font-mono text-[11px] text-zinc-500" title={summary}>
            {summary}
          </p>

          {provider.mirror ? (
            <p className="mt-1 text-[11px] text-amber-300/80">
              Quality-of-life mirror — excluded from safe-to-erase, cleanup, and the dashboard's headline backup counts.
            </p>
          ) : null}

          {provider.enabled && isFailing && lastError ? (
            <div className="mt-1.5 rounded-md border border-amber-500/30 bg-amber-500/10 px-2 py-1.5">
              <p className="text-[11px] text-amber-300" title={lastError.message}>
                Failing — <span className="selectable break-words">{firstLine(lastError.message)}</span>
              </p>
              {reconnectRemote ? (
                <p className="selectable mt-1 font-mono text-[10px] text-amber-200/90">
                  Fix: rclone config reconnect {reconnectRemote}
                </p>
              ) : null}
            </div>
          ) : null}

          <div className="mt-2 flex flex-wrap items-center gap-3">
            {plugin ? (
              <div className="flex flex-wrap items-center gap-1.5">
                {plugin.supportsVerify ? <Chip label="Verify" /> : null}
                {plugin.supportsDelete ? <Chip label="Delete" /> : null}
                {plugin.supportsResume ? <Chip label="Resume" /> : null}
                {plugin.maxFileSize > 0 ? <Chip label={`Max ${formatBytes(plugin.maxFileSize)}`} /> : null}
              </div>
            ) : null}
            <label className="flex items-center gap-1.5 text-[11px] text-zinc-400">
              <span>Upload order</span>
              <select
                value={provider.uploadOrder || "oldest_first"}
                disabled={saving}
                onChange={(e) => void save({ uploadOrder: e.target.value })}
                className="rounded-md border border-zinc-700 bg-zinc-950 px-2 py-1 text-[11px] text-zinc-200 outline-none focus:border-blue-500"
              >
                {ORDER_OPTIONS.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </select>
            </label>
          </div>

          {backfill ? (
            <div className="mt-3 rounded-md border border-blue-500/30 bg-blue-500/5 p-3">
              <div className="flex items-center gap-2 text-[12px] text-blue-200/90">
                <CloudArrowUpIcon className="h-4 w-4 flex-none" />
                <span>
                  Queueing missing backups… {formatNumber(backfill.done)}
                  {backfill.total > 0 ? ` / ${formatNumber(backfill.total)}` : ""}
                </span>
              </div>
              <ProgressBar
                percent={backfill.total > 0 ? (backfill.done / backfill.total) * 100 : null}
                size="sm"
                className="mt-2"
              />
            </div>
          ) : provider.enabled && missing > 0 ? (
            <div className="mt-3 flex flex-wrap items-center gap-3 rounded-md border border-blue-500/30 bg-blue-500/5 p-3">
              <div className="min-w-0 flex-1 text-[12px] text-blue-200/90">
                <span className="font-medium text-blue-100">Back up your existing library?</span>{" "}
                <span>
                  {formatNumber(missing)} asset{missing === 1 ? "" : "s"} aren’t queued for this destination yet.
                </span>
              </div>
              <Button
                size="sm"
                variant="primary"
                icon={CloudArrowUpIcon}
                disabled={backfillBusy}
                onClick={onQueueBackfill}
              >
                Queue {formatNumber(missing)} backup{missing === 1 ? "" : "s"}
              </Button>
            </div>
          ) : null}
        </div>
        <div className="flex-none">
          <Button
            size="sm"
            variant={provider.enabled ? "secondary" : "primary"}
            loading={saving}
            onClick={() => void save({ enabled: !provider.enabled })}
          >
            {provider.enabled ? "Disable" : "Enable"}
          </Button>
        </div>
      </div>
    </Card>
  );
}

function Chip({ label }: { label: string }) {
  return (
    <span className="rounded-full bg-zinc-800/70 px-2 py-0.5 text-[10px] font-medium text-zinc-400 ring-1 ring-zinc-700/40 ring-inset">
      {label}
    </span>
  );
}

function AddDestination({
  plugins,
  onCancel,
  onAdded,
}: {
  plugins: PluginDTO[];
  onCancel: () => void;
  onAdded: () => void;
}) {
  const toast = useToast();
  const [pluginName, setPluginName] = useState(plugins[0]?.name ?? "localfs");
  const [root, setRoot] = useState("");
  const [rawConfig, setRawConfig] = useState("{}");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Mirror + upload order (shared across plugins).
  const [mirror, setMirror] = useState(false);
  const [uploadOrder, setUploadOrder] = useState("oldest_first");

  // rclone state.
  const [rclone, setRclone] = useState<RcloneRemotesDTO | null>(null);
  const [rcloneLoading, setRcloneLoading] = useState(false);
  const [remote, setRemote] = useState("");
  const [poolRemotes, setPoolRemotes] = useState<string[]>([]);
  const [rclonePath, setRclonePath] = useState("PAIM-Backup");
  const [remoteInfo, setRemoteInfo] = useState<RcloneRemoteInfoDTO | null>(null);

  const isLocalfs = pluginName === "localfs";
  const isRclone = pluginName === "rclone";

  // Toggling mirror on defaults the order to newest_first (new imports jump the
  // queue); toggling off restores oldest_first.
  const setMirrorAndOrder = (on: boolean) => {
    setMirror(on);
    setUploadOrder(on ? "newest_first" : "oldest_first");
    if (on && isRclone && poolRemotes.length === 0 && remote) setPoolRemotes([remote]);
  };

  const loadRemotes = async () => {
    setRcloneLoading(true);
    try {
      const res = await ProviderService.RcloneRemotes();
      setRclone(res);
      const remotes = res.remotes ?? [];
      if (remotes.length > 0) {
        setRemote((cur) => (cur && remotes.includes(cur) ? cur : (remotes[0] ?? "")));
      }
    } catch (e) {
      toast.fromError(e, "Could not query rclone");
    } finally {
      setRcloneLoading(false);
    }
  };

  // Probe rclone install status whenever the rclone plugin is selected.
  useEffect(() => {
    if (isRclone && rclone === null && !rcloneLoading) {
      void loadRemotes();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isRclone]);

  // When the single remote changes, probe its checksum support: a backend with no
  // usable content hash (e.g. Google Photos) cannot be verified, so pre-suggest the
  // Mirror toggle and warn.
  useEffect(() => {
    if (!isRclone || !remote) {
      setRemoteInfo(null);
      return;
    }
    let cancelled = false;
    void (async () => {
      try {
        const info = await ProviderService.RcloneRemoteInfo(remote);
        if (cancelled) return;
        setRemoteInfo(info);
        if (!info.supportsChecksum) setMirrorAndOrder(true);
      } catch {
        if (!cancelled) setRemoteInfo(null);
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isRclone, remote]);

  const pick = async () => {
    try {
      const picked = await CleanupService.PickFolder();
      if (picked) setRoot(picked);
    } catch (e) {
      toast.fromError(e, "Could not open folder picker");
    }
  };

  const submit = async () => {
    setError(null);
    if (isLocalfs && !root.trim()) {
      setError("Choose a root folder for the local filesystem destination.");
      return;
    }
    const pool = poolRemotes.filter((r) => r.trim());
    if (isRclone && mirror && pool.length === 0) {
      setError("Add at least one rclone remote to the mirror pool.");
      return;
    }
    if (isRclone && !mirror && !remote.trim()) {
      setError("Choose an rclone remote for this destination.");
      return;
    }
    let configJSON: string;
    if (isLocalfs) {
      configJSON = JSON.stringify({ root: root.trim() });
    } else if (isRclone) {
      configJSON = mirror
        ? JSON.stringify({ remotes: pool, path: rclonePath.trim() || "PAIM-Backup" })
        : JSON.stringify({ remote: remote.trim(), path: rclonePath.trim() || "PAIM-Backup" });
    } else {
      configJSON = rawConfig.trim() || "{}";
    }
    setSaving(true);
    try {
      await ProviderService.Add(pluginName, configJSON, mirror, uploadOrder);
      toast.success("Destination added");
      onAdded();
    } catch (e) {
      setError(errText(e));
    } finally {
      setSaving(false);
    }
  };

  const rcloneReady = isRclone && rclone?.installed === true;
  const noChecksum = isRclone && remoteInfo != null && !remoteInfo.supportsChecksum;
  // Google Photos' rclone backend maps date folders to (nested) albums, so the
  // destination-path field is an album-name prefix rather than a plain folder.
  const isGooglePhotos = isRclone && remoteInfo?.backendType === "googlephotos";

  return (
    <Card title="Add destination" subtitle="Validated against the plugin before it is saved.">
      <div className="space-y-3">
        <label className="block">
          <span className="text-xs font-medium text-zinc-400">Plugin</span>
          <select
            value={pluginName}
            onChange={(e) => {
              setPluginName(e.target.value);
              setError(null);
            }}
            className="mt-1.5 w-full rounded-md border border-zinc-700 bg-zinc-950 px-3 py-1.5 text-[13px] text-zinc-200 outline-none focus:border-blue-500"
          >
            {plugins.length === 0 ? <option value="localfs">localfs</option> : null}
            {plugins.map((p) => (
              <option key={p.name} value={p.name}>
                {p.name}
              </option>
            ))}
          </select>
        </label>

        {isLocalfs ? (
          <label className="block">
            <span className="text-xs font-medium text-zinc-400">Root folder</span>
            <div className="mt-1.5 flex items-center gap-2">
              <Button icon={FolderOpenIcon} variant="secondary" size="sm" onClick={pick}>
                Choose…
              </Button>
              <input
                value={root}
                onChange={(e) => setRoot(e.target.value)}
                placeholder="/Volumes/Backup/PAIM"
                className="min-w-0 flex-1 rounded-md border border-zinc-700 bg-zinc-950 px-3 py-1.5 font-mono text-[12px] text-zinc-200 outline-none focus:border-blue-500"
              />
            </div>
          </label>
        ) : isRclone ? (
          <RcloneConfig
            status={rclone}
            loading={rcloneLoading}
            mirror={mirror}
            remote={remote}
            onRemote={setRemote}
            poolRemotes={poolRemotes}
            onPoolRemotes={setPoolRemotes}
            path={rclonePath}
            onPath={setRclonePath}
            onRetry={() => void loadRemotes()}
            remoteInfo={remoteInfo}
            isGooglePhotos={isGooglePhotos}
          />
        ) : (
          <label className="block">
            <span className="text-xs font-medium text-zinc-400">Config (JSON)</span>
            <textarea
              value={rawConfig}
              onChange={(e) => setRawConfig(e.target.value)}
              rows={4}
              className="mt-1.5 w-full rounded-md border border-zinc-700 bg-zinc-950 px-3 py-1.5 font-mono text-[12px] text-zinc-200 outline-none focus:border-blue-500"
            />
          </label>
        )}

        {noChecksum ? (
          <div className="flex items-start gap-2 rounded-md border border-amber-500/30 bg-amber-500/5 p-3 text-[12px] text-amber-200/90">
            <ExclamationTriangleIcon className="mt-0.5 h-4 w-4 flex-none" />
            <span>
              This destination’s backend{remoteInfo?.backendType ? ` (${remoteInfo.backendType})` : ""} exposes no
              checksum — uploads to it cannot be verified. It has been marked as a mirror (quality-of-life) destination.
            </span>
          </div>
        ) : null}

        {/* Mirror toggle + explanatory copy. */}
        <label className="flex items-start gap-2 rounded-md border border-zinc-800 bg-zinc-900/40 p-3 text-[12px] text-zinc-300">
          <input
            type="checkbox"
            checked={mirror}
            onChange={(e) => setMirrorAndOrder(e.target.checked)}
            className="mt-0.5 h-4 w-4 flex-none accent-blue-600"
          />
          <span>
            <span className="font-medium text-zinc-200">Mirror (quality-of-life) provider</span>
            <span className="mt-0.5 block text-zinc-500">
              A mirror never blocks a safety verdict: its jobs are excluded from safe-to-erase, cleanup, the source-clear
              gate, and the dashboard's headline backup counts. Verification is best-effort. Use it for convenience
              destinations like Google Photos.
            </span>
          </span>
        </label>

        <label className="block">
          <span className="text-xs font-medium text-zinc-400">Upload order</span>
          <select
            value={uploadOrder}
            onChange={(e) => setUploadOrder(e.target.value)}
            className="mt-1.5 w-full rounded-md border border-zinc-700 bg-zinc-950 px-3 py-1.5 text-[13px] text-zinc-200 outline-none focus:border-blue-500"
          >
            {ORDER_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
          <span className="mt-1 block text-[11px] text-zinc-500">
            Newest photos upload first — new imports jump the queue (good for a quota-limited mirror).
          </span>
        </label>

        {error ? (
          <p className="rounded-md border border-red-500/30 bg-red-500/5 px-3 py-2 text-[12px] text-red-400">{error}</p>
        ) : null}

        <div className="flex items-center justify-end gap-2">
          <Button variant="ghost" onClick={onCancel} disabled={saving}>
            Cancel
          </Button>
          <Button
            variant="primary"
            icon={CheckCircleIcon}
            onClick={() => void submit()}
            loading={saving}
            disabled={isRclone && !rcloneReady}
          >
            Add destination
          </Button>
        </div>
      </div>
    </Card>
  );
}

/** RcloneConfig renders install status, a remotes dropdown (or an ordered mirror
 *  pool), and a destination path field for the rclone plugin. */
function RcloneConfig({
  status,
  loading,
  mirror,
  remote,
  onRemote,
  poolRemotes,
  onPoolRemotes,
  path,
  onPath,
  onRetry,
  remoteInfo,
  isGooglePhotos,
}: {
  status: RcloneRemotesDTO | null;
  loading: boolean;
  mirror: boolean;
  remote: string;
  onRemote: (v: string) => void;
  poolRemotes: string[];
  onPoolRemotes: (v: string[]) => void;
  path: string;
  onPath: (v: string) => void;
  onRetry: () => void;
  remoteInfo: RcloneRemoteInfoDTO | null;
  isGooglePhotos: boolean;
}) {
  if (loading || status === null) {
    return <LoadingBlock label="Checking rclone…" />;
  }

  if (!status.installed) {
    return (
      <div className="space-y-2 rounded-md border border-amber-500/30 bg-amber-500/5 p-3 text-[12px] text-amber-200/90">
        <p className="font-medium">rclone is not installed</p>
        <p>
          PAIM uses <span className="font-mono">rclone</span> to back up to Google Drive and other cloud remotes. Install
          it, then configure a remote:
        </p>
        <pre className="selectable overflow-x-auto rounded bg-zinc-950/60 p-2 font-mono text-[11px] text-zinc-300">
          brew install rclone{"\n"}rclone config
        </pre>
        <Button icon={ArrowPathIcon} variant="secondary" size="sm" onClick={onRetry}>
          Re-check
        </Button>
      </div>
    );
  }

  const remotes = status.remotes ?? [];

  if (remotes.length === 0) {
    return (
      <div className="space-y-2 rounded-md border border-amber-500/30 bg-amber-500/5 p-3 text-[12px] text-amber-200/90">
        <p className="font-medium">No rclone remotes configured</p>
        <p>
          Run <span className="font-mono">rclone config</span> to add one (choose <span className="font-mono">drive</span>{" "}
          for Google Drive), then re-check.
        </p>
        <Button icon={ArrowPathIcon} variant="secondary" size="sm" onClick={onRetry}>
          Re-check
        </Button>
      </div>
    );
  }

  return (
    <div className="space-y-3">
      {status.error ? (
        <p className="rounded-md border border-red-500/30 bg-red-500/5 px-3 py-2 text-[12px] text-red-400">
          {status.error}
        </p>
      ) : null}

      {mirror ? (
        <RclonePool remotes={remotes} pool={poolRemotes} onPool={onPoolRemotes} />
      ) : (
        <label className="block">
          <span className="text-xs font-medium text-zinc-400">Remote</span>
          <select
            value={remote}
            onChange={(e) => onRemote(e.target.value)}
            className="mt-1.5 w-full rounded-md border border-zinc-700 bg-zinc-950 px-3 py-1.5 font-mono text-[12px] text-zinc-200 outline-none focus:border-blue-500"
          >
            {remotes.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </select>
          {remoteInfo?.backendType ? (
            <span className="mt-1 block text-[11px] text-zinc-500">Backend: {remoteInfo.backendType}</span>
          ) : null}
        </label>
      )}

      <label className="block">
        <span className="text-xs font-medium text-zinc-400">
          {isGooglePhotos ? "Album name prefix" : "Destination path"}
        </span>
        <input
          value={path}
          onChange={(e) => onPath(e.target.value)}
          placeholder="PAIM-Backup"
          className="mt-1.5 w-full rounded-md border border-zinc-700 bg-zinc-950 px-3 py-1.5 font-mono text-[12px] text-zinc-200 outline-none focus:border-blue-500"
        />
        <span className="mt-1 block text-[11px] text-zinc-500">
          {isGooglePhotos
            ? "Your date folders become albums (e.g. PAIM-Backup/2019/2019-06-12 Trip) — one album per event, sidestepping Google Photos' per-album item cap."
            : "Folder within each remote where the archive tree is mirrored."}
        </span>
      </label>
    </div>
  );
}

/** RclonePool edits an ordered list of remotes for a mirror pool. Each remote —
 *  typically backed by its own Google Cloud project — has an independent daily
 *  quota; PAIM fails over automatically when one is exhausted. */
function RclonePool({
  remotes,
  pool,
  onPool,
}: {
  remotes: string[];
  pool: string[];
  onPool: (v: string[]) => void;
}) {
  const rows = pool.length > 0 ? pool : [remotes[0] ?? ""];
  const setAt = (i: number, v: string) => {
    const next = [...rows];
    next[i] = v;
    onPool(next);
  };
  const removeAt = (i: number) => onPool(rows.filter((_, idx) => idx !== i));
  const add = () => onPool([...rows, remotes.find((r) => !rows.includes(r)) ?? (remotes[0] ?? "")]);

  return (
    <div className="space-y-2">
      <span className="text-xs font-medium text-zinc-400">Remote pool (ordered)</span>
      <p className="text-[11px] text-zinc-500">
        Each remote backed by its own Google Cloud project has an independent daily quota — PAIM fails over automatically
        when one is exhausted.
      </p>
      {rows.map((r, i) => (
        <div key={i} className="flex items-center gap-2">
          <span className="w-5 text-right text-[11px] text-zinc-600">{i + 1}.</span>
          <select
            value={r}
            onChange={(e) => setAt(i, e.target.value)}
            className="min-w-0 flex-1 rounded-md border border-zinc-700 bg-zinc-950 px-3 py-1.5 font-mono text-[12px] text-zinc-200 outline-none focus:border-blue-500"
          >
            {remotes.map((opt) => (
              <option key={opt} value={opt}>
                {opt}
              </option>
            ))}
          </select>
          <Button
            size="sm"
            variant="ghost"
            icon={TrashIcon}
            disabled={rows.length <= 1}
            onClick={() => removeAt(i)}
          >
            Remove
          </Button>
        </div>
      ))}
      <Button size="sm" variant="secondary" icon={PlusIcon} onClick={add} disabled={rows.length >= remotes.length}>
        Add remote
      </Button>
    </div>
  );
}

/** Parse a provider's ConfigJSON defensively for display. */
// firstLine returns the first line of an error message, trimmed and capped so the
// failing-state summary stays compact (the full text is in the tooltip).
function firstLine(msg: string): string {
  const line = (msg ?? "").split("\n")[0].trim();
  return line.length > 120 ? `${line.slice(0, 117)}…` : line;
}

function configSummary(configJson: string): string {
  if (!configJson) return "No configuration";
  try {
    const parsed = JSON.parse(configJson) as Record<string, unknown>;
    if (typeof parsed.root === "string" && parsed.root) return parsed.root;
    // rclone pool: show "gp1:, gp2: → path".
    if (Array.isArray(parsed.remotes) && parsed.remotes.length > 0) {
      const rems = (parsed.remotes as unknown[]).map((r) => String(r)).join(", ");
      const p = typeof parsed.path === "string" ? parsed.path.replace(/^\/+/, "") : "";
      return p ? `${rems} → ${p}` : rems;
    }
    if (typeof parsed.remote === "string" && parsed.remote) {
      const rem = parsed.remote.endsWith(":") ? parsed.remote : `${parsed.remote}:`;
      const p = typeof parsed.path === "string" ? parsed.path.replace(/^\/+/, "") : "";
      return p ? `${rem}${p}` : rem;
    }
    const entries = Object.entries(parsed).filter(([k]) => k !== "mirror");
    if (entries.length === 0) return "No configuration";
    return entries.map(([k, v]) => `${k}: ${String(v)}`).join(" · ");
  } catch {
    return configJson;
  }
}

function errText(err: unknown): string {
  if (err instanceof Error) return err.message;
  if (typeof err === "string") return err;
  if (err && typeof err === "object" && "message" in err) {
    const m = (err as { message?: unknown }).message;
    if (typeof m === "string") return m;
  }
  return "Configuration was rejected by the plugin.";
}
