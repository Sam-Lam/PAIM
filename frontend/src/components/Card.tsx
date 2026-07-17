import type { ReactNode } from "react";

export interface CardProps {
  children: ReactNode;
  className?: string;
  /** Optional header row. */
  title?: ReactNode;
  subtitle?: ReactNode;
  /** Optional right-aligned header content (buttons, badges). */
  actions?: ReactNode;
  /** Removes inner padding (e.g. when embedding a table). */
  flush?: boolean;
}

export function Card({ children, className = "", title, subtitle, actions, flush = false }: CardProps) {
  const hasHeader = title != null || actions != null;
  return (
    <section
      className={`rounded-lg border border-zinc-800 bg-zinc-900/60 ${className}`}
    >
      {hasHeader ? (
        <header className="flex items-start justify-between gap-3 border-b border-zinc-800 px-4 py-3">
          <div className="min-w-0">
            {title != null ? <h2 className="text-[13px] font-semibold text-zinc-100">{title}</h2> : null}
            {subtitle != null ? <p className="mt-0.5 text-xs text-zinc-500">{subtitle}</p> : null}
          </div>
          {actions != null ? <div className="flex flex-none items-center gap-2">{actions}</div> : null}
        </header>
      ) : null}
      <div className={flush ? "" : "p-4"}>{children}</div>
    </section>
  );
}
