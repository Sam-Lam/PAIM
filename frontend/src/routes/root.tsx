import { Outlet, createRootRoute } from "@tanstack/react-router";
import { Sidebar } from "../components";

function RootLayout() {
  return (
    <div className="flex h-screen w-screen overflow-hidden bg-zinc-950 text-zinc-100">
      <Sidebar />
      <main className="flex-1 overflow-y-auto">
        {/* Draggable strip under the macOS titlebar; page headers opt into drag. */}
        <div className="mx-auto max-w-6xl px-6 py-5 pt-8">
          <Outlet />
        </div>
      </main>
    </div>
  );
}

export const rootRoute = createRootRoute({ component: RootLayout });
