// Central re-export of the generated Wails bindings. Every service call and
// every DTO type used in the frontend flows through here so the (long,
// generated) import path lives in exactly one place. NEVER call fetch/axios —
// all backend access is through these bindings.
export {
  BackupService,
  CleanupService,
  DashboardService,
  DuplicateService,
  HistoryService,
  ImportService,
  LogService,
  ProviderService,
  SettingsService,
  SourcesService,
} from "../../bindings/github.com/autolinepro/paim/internal/services";

export {
  AssetDTO,
  BackupJobDTO,
  BackupProgress,
  BackupQueueChanged,
  BackupSummaryDTO,
  ClassStatDTO,
  CleanupReportDTO,
  DashboardStats,
  DryRunReportDTO,
  DuplicatePairDTO,
  ImportCompleted,
  ImportOptions,
  ImportProgress,
  LogEntryDTO,
  LogEntryEvent,
  MatchDTO,
  MonthCountDTO,
  PageResult,
  PluginDTO,
  ProviderDTO,
  QueueSummaryDTO,
  RecommendationDTO,
  SafeToEraseDTO,
  ScanSummary,
  SessionDTO,
  SessionDetail,
  Settings,
  SourceDTO,
  SourceIdentified,
  StartImportResult,
  TotalsDTO,
  VolumeDTO,
  VolumeEvent,
} from "../../bindings/github.com/autolinepro/paim/internal/services";

// Canonical event names emitted by the Go services (internal/services/events.go).
export const WailsEvents = {
  ImportProgress: "import:progress",
  ImportCompleted: "import:completed",
  BackupProgress: "backup:progress",
  BackupQueueChanged: "backup:queue-changed",
  VolumeMounted: "volume:mounted",
  VolumeUnmounted: "volume:unmounted",
  SourceIdentified: "source:identified",
  LogEntry: "log:entry",
} as const;
