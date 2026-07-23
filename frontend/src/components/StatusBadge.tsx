export type BadgeTone = "success" | "warn" | "danger" | "info" | "neutral" | "muted";

const TONE_CLASS: Record<BadgeTone, string> = {
  success: "bg-emerald-500/10 text-emerald-400 ring-emerald-500/25",
  warn: "bg-amber-500/10 text-amber-400 ring-amber-500/25",
  danger: "bg-red-500/10 text-red-400 ring-red-500/25",
  info: "bg-blue-500/10 text-blue-400 ring-blue-500/25",
  neutral: "bg-zinc-500/10 text-zinc-300 ring-zinc-500/25",
  muted: "bg-zinc-800/60 text-zinc-500 ring-zinc-700/40",
};

interface Mapping {
  tone: BadgeTone;
  label: string;
}

// Every domain status string across Asset verification/backup, ImportSession,
// and BackupJob. Keyed by the lowercased raw value.
const STATUS_MAP: Record<string, Mapping> = {
  // verification
  verified: { tone: "success", label: "Verified" },
  verifying: { tone: "info", label: "Verifying" },
  // backup status
  none: { tone: "muted", label: "None" },
  partial: { tone: "warn", label: "Partial" },
  complete: { tone: "success", label: "Complete" },
  // session / job lifecycle
  scanning: { tone: "info", label: "Scanning" },
  dry_run: { tone: "info", label: "Dry Run" },
  running: { tone: "info", label: "Running" },
  pending: { tone: "warn", label: "Pending" },
  paused: { tone: "neutral", label: "Paused" },
  completed: { tone: "success", label: "Completed" },
  failed: { tone: "danger", label: "Failed" },
  cancelled: { tone: "neutral", label: "Cancelled" },
  canceled: { tone: "neutral", label: "Cancelled" },
  interrupted: { tone: "warn", label: "Interrupted" },
  // import modes (shown as badges in history)
  copy: { tone: "info", label: "Copy" },
  adopt: { tone: "neutral", label: "Adopt in place" },
};

function titleCase(s: string): string {
  return s
    .replace(/[_-]+/g, " ")
    .replace(/\b\w/g, (c) => c.toUpperCase())
    .trim();
}

export interface StatusBadgeProps {
  status: string;
  /** Force a tone regardless of the mapping. */
  tone?: BadgeTone;
  /** Override the displayed label. */
  label?: string;
  /** Show a leading dot. */
  dot?: boolean;
  className?: string;
}

export function StatusBadge({ status, tone, label, dot = false, className = "" }: StatusBadgeProps) {
  const key = (status ?? "").toLowerCase();
  const mapped = STATUS_MAP[key];
  const resolvedTone: BadgeTone = tone ?? mapped?.tone ?? "neutral";
  const resolvedLabel = label ?? mapped?.label ?? (status ? titleCase(status) : "—");
  const dotColor: Record<BadgeTone, string> = {
    success: "bg-emerald-400",
    warn: "bg-amber-400",
    danger: "bg-red-400",
    info: "bg-blue-400",
    neutral: "bg-zinc-400",
    muted: "bg-zinc-500",
  };
  return (
    <span
      className={`inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium ring-1 ring-inset whitespace-nowrap ${TONE_CLASS[resolvedTone]} ${className}`}
    >
      {dot ? <span className={`h-1.5 w-1.5 rounded-full ${dotColor[resolvedTone]}`} /> : null}
      {resolvedLabel}
    </span>
  );
}

/**
 * Boolean safe-to-erase badge. Three states: green "Safe to erase", amber "No
 * backup destination" (archived + verified but the archive is the only copy — a
 * distinct, non-alarming state), and red "Not safe to erase" for genuinely
 * unprotected files.
 */
export function SafeToEraseBadge({
  safe,
  noBackupDestination = false,
}: {
  safe: boolean;
  noBackupDestination?: boolean;
}) {
  if (safe) return <StatusBadge status="safe" tone="success" label="Safe to erase" dot />;
  if (noBackupDestination)
    return <StatusBadge status="no-backup" tone="warn" label="No backup destination" dot />;
  return <StatusBadge status="not-safe" tone="danger" label="Not safe to erase" dot />;
}
