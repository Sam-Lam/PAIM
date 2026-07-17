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

  const isLocalfs = pluginName === "localfs";

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
    const configJSON = isLocalfs ? JSON.stringify({ root: root.trim() }) : rawConfig.trim() || "{}";
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
          <Button variant="primary" icon={CheckCircleIcon} onClick={() => void submit()} loading={saving}>
            Add destination
          </Button>
        </div>
      </div>
    </Card>
  );
}

/** Parse a provider's ConfigJSON defensively for display. */
function configSummary(configJson: string): string {
  if (!configJson) return "No configuration";
  try {
    const parsed = JSON.parse(configJson) as Record<string, unknown>;
    if (typeof parsed.root === "string" && parsed.root) return parsed.root;
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
