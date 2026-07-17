// Formatting helpers shared across pages. Pure functions, no React.

/** Human-readable byte size, binary (1024) scale. */
export function formatBytes(bytes: number | null | undefined, digits = 1): string {
  const n = typeof bytes === "number" && isFinite(bytes) ? bytes : 0;
  if (n <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  const i = Math.min(units.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  const value = n / Math.pow(1024, i);
  const rounded = i === 0 ? value : Number(value.toFixed(digits));
  return `${rounded} ${units[i]}`;
}

/** Compact duration from seconds, e.g. "1h 03m", "45s", "2m 05s". */
export function formatDuration(seconds: number | null | undefined): string {
  const s = typeof seconds === "number" && isFinite(seconds) && seconds > 0 ? Math.round(seconds) : 0;
  if (s === 0) return "0s";
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  if (h > 0) return `${h}h ${String(m).padStart(2, "0")}m`;
  if (m > 0) return `${m}m ${String(sec).padStart(2, "0")}s`;
  return `${sec}s`;
}

/** Estimated-time phrasing (rounds up to friendly units). */
export function formatEta(seconds: number | null | undefined): string {
  const s = typeof seconds === "number" && isFinite(seconds) ? seconds : 0;
  if (s <= 0) return "—";
  if (s < 60) return `~${Math.max(1, Math.round(s))}s`;
  if (s < 3600) return `~${Math.round(s / 60)} min`;
  return `~${(s / 3600).toFixed(1)} h`;
}

/** Grouped integer, e.g. 12,345. */
export function formatNumber(n: number | null | undefined): string {
  const v = typeof n === "number" && isFinite(n) ? n : 0;
  return v.toLocaleString(undefined, { maximumFractionDigits: 0 });
}

/** Transfer/throughput rate from bytes-per-second. */
export function formatRate(bytesPerSec: number | null | undefined): string {
  const v = typeof bytesPerSec === "number" && isFinite(bytesPerSec) && bytesPerSec > 0 ? bytesPerSec : 0;
  if (v === 0) return "—";
  return `${formatBytes(v)}/s`;
}

function toDate(value: string | Date | null | undefined): Date | null {
  if (!value) return null;
  const d = value instanceof Date ? value : new Date(value);
  if (isNaN(d.getTime())) return null;
  // Go zero time serializes as 0001-01-01 — treat as "no date".
  if (d.getUTCFullYear() <= 1) return null;
  return d;
}

/** Absolute local date+time, e.g. "Jul 17, 2026, 3:42 PM". */
export function formatDate(value: string | Date | null | undefined): string {
  const d = toDate(value);
  if (!d) return "—";
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  });
}

/** Date only, e.g. "Jul 17, 2026". */
export function formatDateOnly(value: string | Date | null | undefined): string {
  const d = toDate(value);
  if (!d) return "—";
  return d.toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
}

/** Relative time, e.g. "just now", "5m ago", "3d ago". */
export function formatRelative(value: string | Date | null | undefined): string {
  const d = toDate(value);
  if (!d) return "—";
  const diffMs = Date.now() - d.getTime();
  const sec = Math.round(diffMs / 1000);
  if (sec < 0) return formatDate(d);
  if (sec < 45) return "just now";
  const min = Math.round(sec / 60);
  if (min < 60) return `${min}m ago`;
  const hr = Math.round(min / 60);
  if (hr < 24) return `${hr}h ago`;
  const day = Math.round(hr / 24);
  if (day < 30) return `${day}d ago`;
  const mo = Math.round(day / 30);
  if (mo < 12) return `${mo}mo ago`;
  return `${Math.round(mo / 12)}y ago`;
}

/** Turn "2026-07" (MonthCountDTO.month) into "Jul". */
export function formatMonthLabel(month: string): string {
  const m = /^(\d{4})-(\d{2})$/.exec(month);
  if (!m) return month;
  const idx = Number(m[2]) - 1;
  const names = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
  return names[idx] ?? month;
}

/** "2026-07" -> "July 2026" for tooltips. */
export function formatMonthLong(month: string): string {
  const m = /^(\d{4})-(\d{2})$/.exec(month);
  if (!m) return month;
  const d = new Date(Number(m[1]), Number(m[2]) - 1, 1);
  return d.toLocaleDateString(undefined, { month: "long", year: "numeric" });
}

/** Last path segment of an absolute path, for compact display. */
export function baseName(p: string | null | undefined): string {
  if (!p) return "";
  const parts = p.split(/[/\\]/).filter(Boolean);
  return parts[parts.length - 1] ?? p;
}
