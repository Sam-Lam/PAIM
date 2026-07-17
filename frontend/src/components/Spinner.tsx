export interface SpinnerProps {
  /** Pixel size of the spinner. */
  size?: number;
  className?: string;
  /** Accessible label. */
  label?: string;
}

export function Spinner({ size = 18, className = "", label = "Loading" }: SpinnerProps) {
  return (
    <span
      role="status"
      aria-label={label}
      className={`inline-block animate-spin rounded-full border-2 border-zinc-700 border-t-blue-500 ${className}`}
      style={{ width: size, height: size }}
    />
  );
}

/** Centered spinner block for full-panel loading states. */
export function LoadingBlock({ label = "Loading…" }: { label?: string }) {
  return (
    <div className="flex flex-col items-center justify-center gap-3 py-16 text-zinc-500">
      <Spinner size={24} />
      <span className="text-xs">{label}</span>
    </div>
  );
}

/** Simple shimmering skeleton block. */
export function Skeleton({ className = "" }: { className?: string }) {
  return <div className={`animate-pulse rounded-md bg-zinc-800/70 ${className}`} />;
}
