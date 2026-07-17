import type { ComponentType, ReactNode } from "react";
import { InboxIcon } from "@heroicons/react/24/outline";

export interface EmptyStateProps {
  title: string;
  description?: ReactNode;
  icon?: ComponentType<{ className?: string }>;
  /** Optional call-to-action (button/link). */
  action?: ReactNode;
  className?: string;
}

export function EmptyState({ title, description, icon: Icon = InboxIcon, action, className = "" }: EmptyStateProps) {
  return (
    <div className={`flex flex-col items-center justify-center gap-2 px-6 py-12 text-center ${className}`}>
      <div className="mb-1 rounded-full border border-zinc-800 bg-zinc-900 p-3">
        <Icon className="h-6 w-6 text-zinc-600" />
      </div>
      <h3 className="text-sm font-medium text-zinc-300">{title}</h3>
      {description != null ? (
        <p className="max-w-sm text-xs leading-relaxed text-zinc-500">{description}</p>
      ) : null}
      {action != null ? <div className="mt-2">{action}</div> : null}
    </div>
  );
}
