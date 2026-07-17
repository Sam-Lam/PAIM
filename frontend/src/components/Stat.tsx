import type { ComponentType, ReactNode } from "react";

export interface StatProps {
  label: string;
  value: ReactNode;
  /** Small caption under the value (e.g. a secondary unit). */
  hint?: ReactNode;
  /** Optional delta line, colored by tone. */
  delta?: ReactNode;
  deltaTone?: "up" | "down" | "neutral";
  icon?: ComponentType<{ className?: string }>;
  /** Tint the value (e.g. red for failures when non-zero). */
  tone?: "default" | "accent" | "success" | "warn" | "danger";
}

const TONE: Record<NonNullable<StatProps["tone"]>, string> = {
  default: "text-zinc-100",
  accent: "text-blue-400",
  success: "text-emerald-400",
  warn: "text-amber-400",
  danger: "text-red-400",
};

const DELTA_TONE: Record<NonNullable<StatProps["deltaTone"]>, string> = {
  up: "text-emerald-400",
  down: "text-red-400",
  neutral: "text-zinc-500",
};

export function Stat({ label, value, hint, delta, deltaTone = "neutral", icon: Icon, tone = "default" }: StatProps) {
  return (
    <div className="rounded-lg border border-zinc-800 bg-zinc-900/60 p-4">
      <div className="flex items-center justify-between">
        <span className="text-xs font-medium text-zinc-500">{label}</span>
        {Icon ? <Icon className="h-4 w-4 text-zinc-600" /> : null}
      </div>
      <div className={`mt-2 text-2xl font-semibold tracking-tight tabular-nums ${TONE[tone]}`}>{value}</div>
      {hint != null ? <div className="mt-0.5 text-xs text-zinc-500">{hint}</div> : null}
      {delta != null ? <div className={`mt-1 text-xs ${DELTA_TONE[deltaTone]}`}>{delta}</div> : null}
    </div>
  );
}
