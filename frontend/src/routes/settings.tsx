import { useEffect, useState } from "react";
import {
  CheckCircleIcon,
  ExclamationTriangleIcon,
  FolderOpenIcon,
  InformationCircleIcon,
} from "@heroicons/react/24/outline";
import { Button, Card, LoadingBlock, PageHeader, StatusBadge } from "../components";
import { CleanupService, SettingsService, Settings } from "../lib/api";
import { useToast } from "../lib/toast";

const APP_VERSION = "0.1.0";

interface FormState {
  masterLibraryRoot: string;
  importConcurrency: number;
  backupWorkers: number;
  maxRetries: number;
  defaultEventName: string;
}

/** Settings — Master Library location, concurrency/worker/retry counts, and defaults. */
export function SettingsPage() {
  const toast = useToast();
  const [form, setForm] = useState<FormState | null>(null);
  const [metadataAvailable, setMetadataAvailable] = useState(true);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [rootError, setRootError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const s = await SettingsService.GetAll();
        if (cancelled) return;
        setForm({
          masterLibraryRoot: s.masterLibraryRoot,
          importConcurrency: s.importConcurrency,
          backupWorkers: s.backupWorkers,
          maxRetries: s.maxRetries,
          defaultEventName: s.defaultEventName,
        });
        setMetadataAvailable(s.metadataAvailable);
      } catch (e) {
        toast.fromError(e, "Failed to load settings");
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const update = <K extends keyof FormState>(key: K, value: FormState[K]) => {
    setForm((f) => (f ? { ...f, [key]: value } : f));
  };

  const pickRoot = async () => {
    try {
      const picked = await CleanupService.PickFolder();
      if (picked) {
        update("masterLibraryRoot", picked);
        setRootError(null);
      }
    } catch (e) {
      toast.fromError(e, "Could not open folder picker");
    }
  };

  const save = async () => {
    if (!form) return;
    setSaving(true);
    setRootError(null);
    try {
      const saved = await SettingsService.Update(
        new Settings({
          masterLibraryRoot: form.masterLibraryRoot.trim(),
          importConcurrency: clampInt(form.importConcurrency, 1),
          backupWorkers: clampInt(form.backupWorkers, 1),
          maxRetries: clampInt(form.maxRetries, 0),
          defaultEventName: form.defaultEventName.trim(),
          metadataAvailable,
        }),
      );
      setForm({
        masterLibraryRoot: saved.masterLibraryRoot,
        importConcurrency: saved.importConcurrency,
        backupWorkers: saved.backupWorkers,
        maxRetries: saved.maxRetries,
        defaultEventName: saved.defaultEventName,
      });
      setMetadataAvailable(saved.metadataAvailable);
      toast.success("Settings saved");
    } catch (e) {
      // The most common rejection is an invalid Master Library path.
      const msg = toast.fromError(e, "Could not save settings");
      setRootError(msg);
    } finally {
      setSaving(false);
    }
  };

  if (loading || !form) {
    return (
      <div>
        <PageHeader title="Settings" />
        <LoadingBlock label="Loading settings…" />
      </div>
    );
  }

  return (
    <div>
      <PageHeader
        title="Settings"
        description="Configure your Master Library location and how PAIM imports and backs up. Worker, retry, and concurrency changes take effect on the next launch."
        actions={
          <Button variant="primary" icon={CheckCircleIcon} onClick={() => void save()} loading={saving}>
            Save changes
          </Button>
        }
      />

      <div className="space-y-4">
        <Card title="Master Library" subtitle="Where copied imports are archived. Validated when you save.">
          <div className="flex flex-wrap items-center gap-3">
            <Button icon={FolderOpenIcon} variant="secondary" onClick={pickRoot}>
              Choose folder…
            </Button>
            <input
              value={form.masterLibraryRoot}
              onChange={(e) => update("masterLibraryRoot", e.target.value)}
              placeholder="No library folder chosen yet"
              className={`min-w-0 flex-1 rounded-md border bg-zinc-950 px-3 py-1.5 font-mono text-[12px] text-zinc-200 outline-none focus:border-blue-500 ${
                rootError ? "border-red-500/60" : "border-zinc-700"
              }`}
            />
          </div>
          {rootError ? (
            <p className="mt-2 flex items-start gap-1.5 text-[12px] text-red-400">
              <ExclamationTriangleIcon className="mt-0.5 h-4 w-4 flex-none" />
              {rootError}
            </p>
          ) : (
            <p className="mt-2 text-[11px] text-zinc-500">
              An empty root is allowed — you can choose the library later. Copy-mode imports require it.
            </p>
          )}
        </Card>

        <Card title="Import">
          <div className="grid gap-4 sm:grid-cols-2">
            <NumberField
              label="Import concurrency"
              hint="Files hashed/copied in parallel during import."
              value={form.importConcurrency}
              min={1}
              onChange={(v) => update("importConcurrency", v)}
            />
            <TextField
              label="Default event name"
              hint="Pre-fills the event folder name for new imports."
              value={form.defaultEventName}
              placeholder="e.g. Untitled"
              onChange={(v) => update("defaultEventName", v)}
            />
          </div>
        </Card>

        <Card title="Backup">
          <div className="grid gap-4 sm:grid-cols-2">
            <NumberField
              label="Backup workers"
              hint="Concurrent upload workers in the backup queue."
              value={form.backupWorkers}
              min={1}
              onChange={(v) => update("backupWorkers", v)}
            />
            <NumberField
              label="Max retries"
              hint="Attempts per job before it is marked failed (exponential backoff)."
              value={form.maxRetries}
              min={0}
              onChange={(v) => update("maxRetries", v)}
            />
          </div>
          <p className="mt-3 flex items-start gap-1.5 text-[11px] text-zinc-500">
            <InformationCircleIcon className="mt-0.5 h-3.5 w-3.5 flex-none" />
            Worker and retry counts are read at startup — restart PAIM for changes to take effect.
          </p>
        </Card>

        <Card title="About">
          <dl className="grid gap-3 sm:grid-cols-2">
            <AboutRow label="Application" value="Photo Archive Integrity Manager (PAIM)" />
            <AboutRow label="Version" value={APP_VERSION} />
            <div className="flex items-center justify-between gap-2">
              <dt className="text-[12px] text-zinc-500">Metadata (exiftool)</dt>
              <dd>
                <StatusBadge
                  status={metadataAvailable ? "available" : "missing"}
                  tone={metadataAvailable ? "success" : "warn"}
                  label={metadataAvailable ? "Available" : "Not detected"}
                  dot
                />
              </dd>
            </div>
          </dl>
          {!metadataAvailable ? (
            <p className="mt-3 flex items-start gap-1.5 text-[11px] text-amber-300/80">
              <ExclamationTriangleIcon className="mt-0.5 h-3.5 w-3.5 flex-none" />
              exiftool was not found. Imports proceed with reduced metadata (capture date falls back to file modification
              time).
            </p>
          ) : null}
        </Card>
      </div>
    </div>
  );
}

function clampInt(v: number, min: number): number {
  const n = Math.floor(Number(v));
  if (!isFinite(n) || n < min) return min;
  return n;
}

function NumberField({
  label,
  hint,
  value,
  min,
  onChange,
}: {
  label: string;
  hint?: string;
  value: number;
  min: number;
  onChange: (v: number) => void;
}) {
  return (
    <label className="block">
      <span className="text-xs font-medium text-zinc-400">{label}</span>
      <input
        type="number"
        min={min}
        value={value}
        onChange={(e) => onChange(e.target.value === "" ? min : Number(e.target.value))}
        className="mt-1.5 w-full rounded-md border border-zinc-700 bg-zinc-950 px-3 py-1.5 text-[13px] text-zinc-200 outline-none focus:border-blue-500"
      />
      {hint ? <span className="mt-1 block text-[11px] text-zinc-500">{hint}</span> : null}
    </label>
  );
}

function TextField({
  label,
  hint,
  value,
  placeholder,
  onChange,
}: {
  label: string;
  hint?: string;
  value: string;
  placeholder?: string;
  onChange: (v: string) => void;
}) {
  return (
    <label className="block">
      <span className="text-xs font-medium text-zinc-400">{label}</span>
      <input
        value={value}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
        className="mt-1.5 w-full rounded-md border border-zinc-700 bg-zinc-950 px-3 py-1.5 text-[13px] text-zinc-200 outline-none focus:border-blue-500"
      />
      {hint ? <span className="mt-1 block text-[11px] text-zinc-500">{hint}</span> : null}
    </label>
  );
}

function AboutRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center justify-between gap-2">
      <dt className="text-[12px] text-zinc-500">{label}</dt>
      <dd className="text-[12px] text-zinc-300">{value}</dd>
    </div>
  );
}
