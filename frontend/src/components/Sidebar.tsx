import type { ComponentType } from "react";
import { Link } from "@tanstack/react-router";
import {
  ArrowDownTrayIcon,
  ClockIcon,
  Cog6ToothIcon,
  CloudArrowUpIcon,
  DocumentDuplicateIcon,
  DocumentTextIcon,
  HomeIcon,
  PuzzlePieceIcon,
  ServerStackIcon,
  SparklesIcon,
} from "@heroicons/react/24/outline";

interface NavEntry {
  to: string;
  label: string;
  icon: ComponentType<{ className?: string }>;
  exact?: boolean;
}

// Spec order.
const NAV: NavEntry[] = [
  { to: "/", label: "Dashboard", icon: HomeIcon, exact: true },
  { to: "/import", label: "Import", icon: ArrowDownTrayIcon },
  { to: "/sources", label: "Sources", icon: ServerStackIcon },
  { to: "/history", label: "Import History", icon: ClockIcon },
  { to: "/duplicates", label: "Duplicate Manager", icon: DocumentDuplicateIcon },
  { to: "/cleanup", label: "Cleanup Assistant", icon: SparklesIcon },
  { to: "/backup-queue", label: "Backup Queue", icon: CloudArrowUpIcon },
  { to: "/providers", label: "Providers", icon: PuzzlePieceIcon },
  { to: "/logs", label: "Logs", icon: DocumentTextIcon },
  { to: "/settings", label: "Settings", icon: Cog6ToothIcon },
];

export function Sidebar() {
  return (
    <aside
      className="flex h-full w-60 flex-none flex-col border-r border-zinc-800 bg-zinc-900/40"
      // The sidebar sits under the invisible macOS titlebar — draggable as chrome.
      style={{ "--wails-draggable": "drag" } as React.CSSProperties}
    >
      {/* App header — leaves room for the traffic lights via top padding. */}
      <div className="flex items-center gap-2.5 px-4 pt-9 pb-4">
        <div className="flex h-8 w-8 flex-none items-center justify-center rounded-lg bg-blue-600 text-sm font-bold text-white">
          PA
        </div>
        <div className="min-w-0 leading-tight">
          <div className="truncate text-[13px] font-semibold text-zinc-100">PAIM</div>
          <div className="truncate text-[11px] text-zinc-500">Archive Integrity</div>
        </div>
      </div>

      <nav
        className="flex-1 space-y-0.5 overflow-y-auto px-2.5 py-2"
        style={{ "--wails-draggable": "no-drag" } as React.CSSProperties}
      >
        {NAV.map((entry) => {
          const Icon = entry.icon;
          return (
            <Link
              key={entry.to}
              to={entry.to}
              activeOptions={{ exact: entry.exact ?? false }}
              className="group flex items-center gap-3 rounded-md px-2.5 py-2 text-[13px] font-medium text-zinc-400 transition-colors hover:bg-zinc-800/60 hover:text-zinc-100"
              activeProps={{
                className:
                  "group flex items-center gap-3 rounded-md px-2.5 py-2 text-[13px] font-medium bg-blue-600/15 text-blue-300",
              }}
            >
              <Icon className="h-5 w-5 flex-none" />
              <span className="truncate">{entry.label}</span>
            </Link>
          );
        })}
      </nav>

      <div className="px-4 py-3 text-[11px] text-zinc-600">Data integrity first</div>
    </aside>
  );
}
