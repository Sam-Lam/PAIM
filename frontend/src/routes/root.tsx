import { useEffect } from "react";
import { Outlet, createRootRoute, useNavigate, useRouterState } from "@tanstack/react-router";
import { Sidebar, LoadingBlock, QuitGuardDialog } from "../components";
import { LibraryProvider, useLibrary } from "../lib/library";

function Shell() {
  const { current, loading } = useLibrary();
  const navigate = useNavigate();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const onWelcome = pathname === "/welcome";

  // Gate the whole app on an open library: with none open, redirect to Welcome
  // (sidebar hidden); once one opens, leave Welcome for the Dashboard.
  useEffect(() => {
    if (loading) return;
    if (!current && !onWelcome) {
      void navigate({ to: "/welcome" });
    } else if (current && onWelcome) {
      void navigate({ to: "/" });
    }
  }, [loading, current, onWelcome, navigate]);

  if (loading) {
    return (
      <div className="flex h-screen w-screen items-center justify-center bg-zinc-950 text-zinc-100">
        <LoadingBlock label="Opening library…" />
      </div>
    );
  }

  if (!current) {
    // Welcome state: no sidebar, the page owns the full window.
    return (
      <div
        className="h-screen w-screen overflow-y-auto bg-zinc-950 text-zinc-100"
        style={{ "--wails-draggable": "drag" } as React.CSSProperties}
      >
        <Outlet />
      </div>
    );
  }

  return (
    <div className="flex h-screen w-screen overflow-hidden bg-zinc-950 text-zinc-100">
      <Sidebar />
      <main className="flex-1 overflow-y-auto">
        <div className="mx-auto max-w-6xl px-6 py-5 pt-8">
          <Outlet />
        </div>
      </main>
    </div>
  );
}

function RootLayout() {
  return (
    <LibraryProvider>
      <Shell />
      {/* Global quit guard: appears over any page when a quit is attempted with
          long operations in flight. */}
      <QuitGuardDialog />
    </LibraryProvider>
  );
}

export const rootRoute = createRootRoute({ component: RootLayout });
