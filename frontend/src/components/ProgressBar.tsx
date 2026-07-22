import type { ReactNode } from "react";

export type ProgressTone = "accent" | "success" | "warn" | "danger";

const BAR_TONE: Record<ProgressTone, string> = {
  accent: "bg-blue-500",
  success: "bg-emerald-500",
  warn: "bg-amber-500",
  danger: "bg-red-500",
};

export interface ProgressBarProps {
  /** 0–100. Values are clamped. Omit / pass null for indeterminate. */
  percent?: number | null;
  /** Left-aligned label above the bar (e.g. phase or filename). */
  label?: ReactNode;
  /** Right-aligned secondary text above the bar (e.g. rate, "512 / 1024"). */
  detail?: ReactNode;
  tone?: ProgressTone;
  /** Bar thickness. */
  size?: "sm" | "md" | "lg";
  /** Show the numeric percent on the right of the label row. */
  showPercent?: boolean;
  /** Animated candy-stripe while running. */
  striped?: boolean;
  className?: string;
}

const HEIGHT: Record<NonNullable<ProgressBarProps["size"]>, string> = {
  sm: "h-1.5",
  md: "h-2.5",
  lg: "h-3.5",
};

export function ProgressBar({
  percent,
  label,
  detail,
  tone = "accent",
  size = "lg",
  showPercent = true,
  striped = false,
  className = "",
}: ProgressBarProps) {
  const indeterminate = percent == null || !isFinite(percent);
  const pct = indeterminate ? 0 : Math.max(0, Math.min(100, percent));
  const hasHeader = label != null || detail != null || (showPercent && !indeterminate);

  return (
    <div className={className}>
      {hasHeader ? (
        <div className="mb-1.5 flex items-center justify-between gap-3 text-xs">
          <div className="min-w-0 truncate text-zinc-300">{label}</div>
          <div className="flex flex-none items-center gap-2 tabular-nums text-zinc-400">
            {detail}
            {showPercent && !indeterminate ? (
              <span className="font-medium text-zinc-200">{pct >= 100 ? 100 : Math.floor(pct)}%</span>
            ) : null}
          </div>
        </div>
      ) : null}
      <div className={`w-full overflow-hidden rounded-full bg-zinc-800 ${HEIGHT[size]}`}>
        {indeterminate ? (
          <div className={`h-full w-1/3 animate-pulse rounded-full ${BAR_TONE[tone]}`} />
        ) : (
          <div
            className={`h-full rounded-full transition-[width] duration-300 ease-out ${BAR_TONE[tone]} ${
              striped ? "bg-[length:1rem_1rem] [background-image:linear-gradient(45deg,rgba(255,255,255,0.15)_25%,transparent_25%,transparent_50%,rgba(255,255,255,0.15)_50%,rgba(255,255,255,0.15)_75%,transparent_75%,transparent)]" : ""
            }`}
            style={{ width: `${pct}%` }}
          />
        )}
      </div>
    </div>
  );
}
