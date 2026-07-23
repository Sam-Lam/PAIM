import { useEffect, useMemo, useState } from "react";
import {
  ArrowPathIcon,
  CheckCircleIcon,
  CircleStackIcon,
  FolderOpenIcon,
  InformationCircleIcon,
  PlusIcon,
  ServerStackIcon,
} from "@heroicons/react/24/outline";
import { Button, Card, EmptyState, LoadingBlock, PageHeader, StatusBadge } from "../components";
import {
  CleanupService,
  ProviderService,
  type PluginDTO,
  type ProviderDTO,
  type RcloneRemotesDTO,
} from "../lib/api";
import { useAsyncData } from "../lib/hooks";
import { useToast } from "../lib/toast";
import { formatBytes } from "../lib/format";

/** Providers — configure backup destinations (localfs and future plugins). */
export function ProvidersPage() {
  const toast = useToast();
  const providers = useAsyncData(() => ProviderService.List());
  const plugins = useAsyncData(() => ProviderService.AvailablePlugins());
  const [adding, setAdding] = useState(false);

  useEffect(() => {
    void providers.run().catch((e) => toast.fromError(e, "Failed to load providers"));
    void plugins.run().catch(() => undefined);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const pluginByName = useMemo(() => {
    const m = new Map<string, PluginDTO>();
    (plugins.data ?? []).forEach((p) => m.set(p.name, p));
    return m;
  }, [plugins.data]);

  const refresh = () => providers.run({ silent: true });

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
          Every imported asset is automatically enqueued for backup to your enabled destinations. If no destination is
          enabled, backups will wait until one is available.
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
              <ProviderCard key={p.id} provider={p} plugin={pluginByName.get(p.pluginName)} onChanged={refresh} />
            ))
          )}

          {adding ? (
            <AddDestination plugins={plugins.data ?? []} onCancel={() => setAdding(false)} onAdded={() => {
              setAdding(false);
              void refresh();
            }} />
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
}: {
  provider: ProviderDTO;
  plugin: PluginDTO | undefined;
  onChanged: () => void;
}) {
  const toast = useToast();
  const [saving, setSaving] = useState(false);
  const summary = configSummary(provider.configJson);

  const toggle = async () => {
    setSaving(true);
    try {
      await ProviderService.Update(provider.id, provider.configJson, !provider.enabled);
      toast.success(provider.enabled ? "Destination disabled" : "Destination enabled");
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
          <div className="flex items-center gap-2">
            <h3 className="text-sm font-semibold text-zinc-100">{provider.pluginName}</h3>
            <StatusBadge
              status={provider.enabled ? "enabled" : "disabled"}
              tone={provider.enabled ? "success" : "muted"}
              label={provider.enabled ? "Enabled" : "Disabled"}
              dot
            />
          </div>
          <p className="selectable mt-0.5 truncate font-mono text-[11px] text-zinc-500" title={summary}>
            {summary}
          </p>

          {plugin ? (
            <div className="mt-2 flex flex-wrap items-center gap-1.5">
              {plugin.supportsVerify ? <Chip label="Verify" /> : null}
              {plugin.supportsDelete ? <Chip label="Delete" /> : null}
              {plugin.supportsResume ? <Chip label="Resume" /> : null}
              {plugin.maxFileSize > 0 ? <Chip label={`Max ${formatBytes(plugin.maxFileSize)}`} /> : null}
            </div>
          ) : null}
        </div>
        <div className="flex-none">
          <Button
            size="sm"
            variant={provider.enabled ? "secondary" : "primary"}
            loading={saving}
            onClick={() => void toggle()}
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

  // rclone state.
  const [rclone, setRclone] = useState<RcloneRemotesDTO | null>(null);
  const [rcloneLoading, setRcloneLoading] = useState(false);
  const [remote, setRemote] = useState("");
  const [rclonePath, setRclonePath] = useState("PAIM-Backup");

  const isLocalfs = pluginName === "localfs";
  const isRclone = pluginName === "rclone";

  const loadRemotes = async () => {
    setRcloneLoading(true);
    try {
      const res = await ProviderService.RcloneRemotes();
      setRclone(res);
      // Default the remote to the first available one.
      if (res.remotes && res.remotes.length > 0) {
        const remotes = res.remotes ?? [];
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
    if (isRclone && !remote.trim()) {
      setError("Choose an rclone remote for this destination.");
      return;
    }
    let configJSON: string;
    if (isLocalfs) {
      configJSON = JSON.stringify({ root: root.trim() });
    } else if (isRclone) {
      configJSON = JSON.stringify({ remote: remote.trim(), path: rclonePath.trim() || "PAIM-Backup" });
    } else {
      configJSON = rawConfig.trim() || "{}";
    }
    setSaving(true);
    try {
      await ProviderService.Add(pluginName, configJSON);
      toast.success("Destination added");
      onAdded();
    } catch (e) {
      // Initialize probe rejected the config — surface it inline.
      setError(errText(e));
    } finally {
      setSaving(false);
    }
  };

  const rcloneReady = isRclone && rclone?.installed === true;

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
            remote={remote}
            onRemote={setRemote}
            path={rclonePath}
            onPath={setRclonePath}
            onRetry={() => void loadRemotes()}
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

/** RcloneConfig renders install status, a remotes dropdown, and a destination
 *  path field for the rclone plugin. When rclone is missing or has no remotes it
 *  shows first-time setup guidance instead. */
function RcloneConfig({
  status,
  loading,
  remote,
  onRemote,
  path,
  onPath,
  onRetry,
}: {
  status: RcloneRemotesDTO | null;
  loading: boolean;
  remote: string;
  onRemote: (v: string) => void;
  path: string;
  onPath: (v: string) => void;
  onRetry: () => void;
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
        <p>
          For Google Drive choose <span className="font-mono">drive</span> in <span className="font-mono">rclone config</span>{" "}
          and follow the browser sign-in. Then reopen this dialog.
        </p>
        <Button icon={ArrowPathIcon} variant="secondary" size="sm" onClick={onRetry}>
          Re-check
        </Button>
      </div>
    );
  }

  const remotes = status.remotes ?? [];

  return (
    <div className="space-y-3">
      {status.error ? (
        <p className="rounded-md border border-red-500/30 bg-red-500/5 px-3 py-2 text-[12px] text-red-400">
          {status.error}
        </p>
      ) : null}

      {remotes.length === 0 ? (
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
      ) : (
        <>
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
          </label>

          <label className="block">
            <span className="text-xs font-medium text-zinc-400">Destination path</span>
            <input
              value={path}
              onChange={(e) => onPath(e.target.value)}
              placeholder="PAIM-Backup"
              className="mt-1.5 w-full rounded-md border border-zinc-700 bg-zinc-950 px-3 py-1.5 font-mono text-[12px] text-zinc-200 outline-none focus:border-blue-500"
            />
            <span className="mt-1 block text-[11px] text-zinc-500">
              Folder within the remote where the archive tree is mirrored.
            </span>
          </label>
        </>
      )}
    </div>
  );
}

/** Parse a provider's ConfigJSON defensively for display. */
function configSummary(configJson: string): string {
  if (!configJson) return "No configuration";
  try {
    const parsed = JSON.parse(configJson) as Record<string, unknown>;
    if (typeof parsed.root === "string" && parsed.root) return parsed.root;
    // rclone: show "remote:path" (e.g. gdrive:PAIM-Backup).
    if (typeof parsed.remote === "string" && parsed.remote) {
      const rem = parsed.remote.endsWith(":") ? parsed.remote : `${parsed.remote}:`;
      const p = typeof parsed.path === "string" ? parsed.path.replace(/^\/+/, "") : "";
      return p ? `${rem}${p}` : rem;
    }
    const entries = Object.entries(parsed);
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
