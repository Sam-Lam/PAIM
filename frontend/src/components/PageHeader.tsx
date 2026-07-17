import type { ReactNode } from "react";

export interface PageHeaderProps {
  title: string;
  description?: ReactNode;
  /** Right-aligned actions (buttons, refresh, filters). */
  actions?: ReactNode;
  /** Marks the header bar draggable (macOS hidden-inset titlebar). */
  draggable?: boolean;
}

export function PageHeader({ title, description, actions, draggable = true }: PageHeaderProps) {
  return (
    <div
      className="flex items-start justify-between gap-4 pb-5"
      // The top strip of each page sits under the invisible macOS titlebar, so
      // dragging here moves the window. Interactive children opt out via no-drag.
      style={draggable ? ({ "--wails-draggable": "drag" } as React.CSSProperties) : undefined}
    >
      <div className="min-w-0">
        <h1 className="text-lg font-semibold tracking-tight text-zinc-100">{title}</h1>
        {description != null ? (
          <p className="mt-1 max-w-2xl text-[13px] leading-relaxed text-zinc-400">{description}</p>
        ) : null}
      </div>
      {actions != null ? (
        <div
          className="flex flex-none items-center gap-2"
          style={{ "--wails-draggable": "no-drag" } as React.CSSProperties}
        >
          {actions}
        </div>
      ) : null}
    </div>
  );
}
