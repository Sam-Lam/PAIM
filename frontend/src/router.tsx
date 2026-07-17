import { createHashHistory, createRoute, createRouter } from "@tanstack/react-router";
import { rootRoute } from "./routes/root";
import { DashboardPage } from "./routes/dashboard";
import { ImportPage } from "./routes/import";
import { SourcesPage } from "./routes/sources";
import { HistoryPage } from "./routes/history";
import { DuplicatesPage } from "./routes/duplicates";
import { CleanupPage } from "./routes/cleanup";
import { BackupQueuePage } from "./routes/backup-queue";
import { ProvidersPage } from "./routes/providers";
import { LogsPage } from "./routes/logs";
import { SettingsPage } from "./routes/settings";

const dashboardRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  component: DashboardPage,
});

const importRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/import",
  component: ImportPage,
  // Sources page can deep-link here with ?root=<mountPoint>.
  validateSearch: (search: Record<string, unknown>): { root?: string } => ({
    root: typeof search.root === "string" ? search.root : undefined,
  }),
});

const sourcesRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/sources",
  component: SourcesPage,
});

const historyRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/history",
  component: HistoryPage,
});

const duplicatesRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/duplicates",
  component: DuplicatesPage,
});

const cleanupRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/cleanup",
  component: CleanupPage,
});

const backupQueueRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/backup-queue",
  component: BackupQueuePage,
});

const providersRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/providers",
  component: ProvidersPage,
});

const logsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/logs",
  component: LogsPage,
});

const settingsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/settings",
  component: SettingsPage,
});

const routeTree = rootRoute.addChildren([
  dashboardRoute,
  importRoute,
  sourcesRoute,
  historyRoute,
  duplicatesRoute,
  cleanupRoute,
  backupQueueRoute,
  providersRoute,
  logsRoute,
  settingsRoute,
]);

// Hash history is the safest option inside a webview (no server, no deep-link
// path handling required).
export const router = createRouter({
  routeTree,
  history: createHashHistory(),
  defaultPreload: "intent",
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
