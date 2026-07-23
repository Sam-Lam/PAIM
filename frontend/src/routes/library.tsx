import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearch } from "@tanstack/react-router";
import {
  ArrowPathIcon,
  ArrowsPointingInIcon,
  ArrowsPointingOutIcon,
  BoltIcon,
  CheckIcon,
  ChevronDownIcon,
  ChevronLeftIcon,
  ChevronRightIcon,
  ChevronUpDownIcon,
  ChevronUpIcon,
  ClipboardDocumentIcon,
  FilmIcon,
  FolderIcon,
  FolderOpenIcon,
  FunnelIcon,
  MagnifyingGlassIcon,
  MagnifyingGlassMinusIcon,
  MagnifyingGlassPlusIcon,
  PencilSquareIcon,
  PhotoIcon,
  Squares2X2Icon,
  XMarkIcon,
} from "@heroicons/react/24/outline";
import {
  Button,
  Card,
  EmptyState,
  LoadingBlock,
  PageHeader,
  StatusBadge,
} from "../components";
import {
  BrowserService,
  ThumbnailService,
  WailsEvents,
  type AssetDetailDTO,
  type AssetRefDTO,
  type BrowseAssetDTO,
  type BrowseFilters,
  type CameraCountDTO,
  type FolderEntryDTO,
  type FolderListingDTO,
  type MonthCountDTO,
  type ThumbsProgress,
  type WarmupStatusDTO,
  type YearCountDTO,
} from "../lib/api";
import { useAsyncData, useWailsEvent } from "../lib/hooks";
import { useToast } from "../lib/toast";
import {
  formatBytes,
  formatDate,
  formatDateOnly,
  formatDuration,
  formatMonthLong,
  formatNumber,
} from "../lib/format";

const PAGE_SIZE = 60;

const MEDIA_TYPES = [
  { value: "", label: "All types" },
  { value: "photo", label: "Photos" },
  { value: "raw_photo", label: "RAW" },
  { value: "video", label: "Videos" },
  { value: "live_photo_pair", label: "Live Photos" },
];
const VERIFICATION = [
  { value: "", label: "Any verification" },
  { value: "verified", label: "Verified" },
  { value: "pending", label: "Pending" },
  { value: "verifying", label: "Verifying" },
  { value: "failed", label: "Failed" },
];
const BACKUP = [
  { value: "", label: "Any backup" },
  { value: "complete", label: "Complete" },
  { value: "partial", label: "Partial" },
  { value: "pending", label: "Pending" },
  { value: "none", label: "None" },
  { value: "failed", label: "Failed" },
];

const SELECT_CLASS =
  "rounded-md border border-zinc-700 bg-zinc-950 px-2.5 py-1.5 text-[13px] text-zinc-200 outline-none focus:border-blue-500";
const DATE_INPUT_CLASS =
  "rounded-md border border-zinc-700 bg-zinc-950 px-2 py-1.5 text-[13px] text-zinc-200 outline-none focus:border-blue-500 [color-scheme:dark]";
const FIELD_LABEL_CLASS = "mb-1 block text-[11px] font-medium text-zinc-500";

/* -------------------------------------------------------------------------- */
/* Date filter model                                                          */
/* -------------------------------------------------------------------------- */

/**
 * DateSel is the Library's compact date-range selection, persisted per machine.
 * It generalizes the old single-month dropdown: a whole year, a single month
 * (the legacy behavior, reachable as a year's sub-level and via the chart's
 * ?yearMonth deep-link), the two relative presets, or a custom day range. It maps
 * to the backend's effective-date bounds (COALESCE(capture_date, import_date)).
 */
type DateSel =
  | { kind: "any" }
  | { kind: "thisYear" }
  | { kind: "last12" }
  | { kind: "year"; year: string }
  | { kind: "month"; yearMonth: string }
  | { kind: "custom"; from: string; to: string }; // from/to are YYYY-MM-DD (either may be "")

const DATE_SEL_KEY = "paim.library.date";

function loadDateSel(): DateSel {
  try {
    const raw = localStorage.getItem(DATE_SEL_KEY);
    if (raw) {
      const p = JSON.parse(raw) as DateSel;
      if (p && typeof (p as { kind?: unknown }).kind === "string") return p;
    }
  } catch {
    /* ignore malformed persisted value */
  }
  return { kind: "any" };
}
function saveDateSel(s: DateSel): void {
  localStorage.setItem(DATE_SEL_KEY, JSON.stringify(s));
}

interface DateFilterValue {
  captureFrom: string;
  captureTo: string;
  yearMonth: string;
}

function endOfToday(): string {
  const n = new Date();
  const m = String(n.getMonth() + 1).padStart(2, "0");
  const d = String(n.getDate()).padStart(2, "0");
  return `${n.getFullYear()}-${m}-${d}T23:59:59`;
}

/**
 * dateSelToFilter maps a DateSel to the three backend filter fields. A single
 * month uses the legacy yearMonth predicate (capture-month strftime); every other
 * dated mode uses inclusive from/to bounds on the effective date. Whole-year and
 * custom-day boundaries are expanded to the first/last instant so the inclusive
 * server bounds cover the entire period.
 */
function dateSelToFilter(sel: DateSel): DateFilterValue {
  const empty: DateFilterValue = { captureFrom: "", captureTo: "", yearMonth: "" };
  switch (sel.kind) {
    case "any":
      return empty;
    case "thisYear": {
      const y = new Date().getFullYear();
      return { captureFrom: `${y}-01-01T00:00:00`, captureTo: endOfToday(), yearMonth: "" };
    }
    case "last12": {
      const now = new Date();
      const from = new Date(now.getFullYear(), now.getMonth() - 11, 1);
      const fm = String(from.getMonth() + 1).padStart(2, "0");
      return { captureFrom: `${from.getFullYear()}-${fm}-01T00:00:00`, captureTo: endOfToday(), yearMonth: "" };
    }
    case "year":
      return { captureFrom: `${sel.year}-01-01T00:00:00`, captureTo: `${sel.year}-12-31T23:59:59`, yearMonth: "" };
    case "month":
      return { captureFrom: "", captureTo: "", yearMonth: sel.yearMonth };
    case "custom":
      return {
        captureFrom: sel.from ? `${sel.from}T00:00:00` : "",
        captureTo: sel.to ? `${sel.to}T23:59:59` : "",
        yearMonth: "",
      };
  }
}

/** A short human label for the active date selection (for the empty-state summary). */
function dateSelLabel(sel: DateSel): string {
  switch (sel.kind) {
    case "any":
      return "";
    case "thisYear":
      return "this year";
    case "last12":
      return "last 12 months";
    case "year":
      return sel.year;
    case "month":
      return formatMonthLong(sel.yearMonth);
    case "custom":
      return `${sel.from || "…"} – ${sel.to || "…"}`;
  }
}

/**
 * Library — a strictly read-only browse grid that proves what is archived and
 * surfaces provenance at a glance. No editing, ratings, albums, or export: PAIM
 * is an integrity tool, not a DAM.
 */
export function LibraryPage() {
  const toast = useToast();
  // Deep-link: the dashboard's "Assets over time" chart navigates here with
  // ?yearMonth=YYYY-MM to pre-apply the capture-month filter.
  const search = useSearch({ strict: false }) as { yearMonth?: string };

  // Filters. The text query is debounced (300ms) into `query`; the rest apply
  // immediately.
  const [queryInput, setQueryInput] = useState("");
  const [query, setQuery] = useState("");
  const [mediaType, setMediaType] = useState("");
  const [verification, setVerification] = useState("");
  const [backup, setBackup] = useState("");
  // Camera exact-match filter: the selected make/model pair ("" = any camera).
  const [camera, setCamera] = useState<{ make: string; model: string }>({ make: "", model: "" });
  // Date-range selection (persisted). A ?yearMonth deep-link pins the month level.
  const [dateSel, setDateSel] = useState<DateSel>(() =>
    search.yearMonth ? { kind: "month", yearMonth: search.yearMonth } : loadDateSel(),
  );
  const setDatePersisted = useCallback((s: DateSel) => {
    saveDateSel(s);
    setDateSel(s);
  }, []);
  const dateFilter = useMemo(() => dateSelToFilter(dateSel), [dateSel]);
  // Tile rendering: crop to square (cover) or fit within it (contain). Persisted per machine.
  const [fitTiles, setFitTiles] = useState(() => localStorage.getItem("paim.library.fit") === "1");
  const toggleFit = () => {
    setFitTiles((v) => {
      localStorage.setItem("paim.library.fit", v ? "0" : "1");
      return !v;
    });
  };
  // View: the flat capture-month grid, or the archive folder tree. Persisted per machine.
  const [view, setView] = useState<"grid" | "folders">(
    () => (localStorage.getItem("paim.library.view") === "folders" ? "folders" : "grid"),
  );
  const setViewPersisted = (v: "grid" | "folders") => {
    localStorage.setItem("paim.library.view", v);
    setView(v);
  };

  // Follow a ?yearMonth deep-link that arrives (or changes) while mounted: apply
  // the month filter and switch to the grid view where that filter lives.
  useEffect(() => {
    const ym = search.yearMonth;
    if (ym) {
      setDateSel({ kind: "month", yearMonth: ym });
      setView("grid");
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [search.yearMonth]);

  // Accumulated grid state ("Load more" pagination).
  const [items, setItems] = useState<BrowseAssetDTO[]>([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(0);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);

  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [menu, setMenu] = useState<ContextMenuState>(null);

  const revealAsset = useCallback(
    async (id: string) => {
      try {
        await BrowserService.RevealAsset(id, "archive");
      } catch (e) {
        toast.fromError(e, "Could not reveal in Finder");
      }
    },
    [toast],
  );

  // Thumbnail warm-up state, shown compactly in the header while running.
  const [warm, setWarm] = useState<WarmupStatusDTO | null>(null);
  const [startingWarm, setStartingWarm] = useState(false);
  useEffect(() => {
    let cancelled = false;
    ThumbnailService.WarmupStatus()
      .then((w) => {
        if (!cancelled) setWarm(w);
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, []);
  useWailsEvent<ThumbsProgress>(WailsEvents.ThumbsProgress, (p) => {
    setWarm({ running: p.running, done: p.done, total: p.total, label: p.label });
  });
  const warmAll = async () => {
    setStartingWarm(true);
    try {
      const st = await ThumbnailService.StartWarmupAll();
      setWarm(st);
    } catch (e) {
      toast.fromError(e, "Could not start thumbnail warm-up");
    } finally {
      setStartingWarm(false);
    }
  };
  const warmRunning = !!warm?.running;

  const months = useAsyncData(() => BrowserService.Months());
  const years = useAsyncData(() => BrowserService.Years());
  const cameras = useAsyncData(() => BrowserService.Cameras());

  const filters = useMemo<BrowseFilters>(
    () => ({
      query,
      mediaType,
      verificationStatus: verification,
      backupStatus: backup,
      sessionId: "",
      yearMonth: dateFilter.yearMonth,
      captureFrom: dateFilter.captureFrom,
      captureTo: dateFilter.captureTo,
      cameraMake: camera.make,
      cameraModel: camera.model,
    }),
    [query, mediaType, verification, backup, dateFilter, camera],
  );

  // Debounce the text query.
  useEffect(() => {
    const id = setTimeout(() => setQuery(queryInput), 300);
    return () => clearTimeout(id);
  }, [queryInput]);

  const load = useCallback(
    async (targetPage: number, reset: boolean) => {
      if (reset) setLoading(true);
      else setLoadingMore(true);
      try {
        const res = await BrowserService.ListAssets(filters, targetPage, PAGE_SIZE);
        setTotal(res.total ?? 0);
        const incoming = res.items ?? [];
        setItems((prev) => (reset ? incoming : [...prev, ...incoming]));
        setPage(targetPage);
      } catch (e) {
        toast.fromError(e, "Failed to load library");
      } finally {
        setLoading(false);
        setLoadingMore(false);
      }
    },
    [filters, toast],
  );

  // Reload from page 1 whenever a filter changes.
  useEffect(() => {
    void load(1, true);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [query, mediaType, verification, backup, dateSel, camera]);

  useEffect(() => {
    void months.run().catch(() => undefined);
    void years.run().catch(() => undefined);
    void cameras.run().catch(() => undefined);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const dateActive = dateSel.kind !== "any";
  const cameraActive = !!camera.make || !!camera.model;
  const activeFilters = useMemo(() => {
    const parts: string[] = [];
    if (query) parts.push(`search “${query}”`);
    if (mediaType) parts.push(`type ${mediaTypeLabel(mediaType)}`);
    if (verification) parts.push(`verification ${verification}`);
    if (backup) parts.push(`backup ${backup}`);
    if (cameraActive) parts.push(`camera ${[camera.make, camera.model].filter(Boolean).join(" ")}`);
    if (dateActive) parts.push(`date ${dateSelLabel(dateSel)}`);
    return parts;
  }, [query, mediaType, verification, backup, cameraActive, camera, dateActive, dateSel]);
  const filtersActive = activeFilters.length > 0;
  const activeCount = activeFilters.length;
  const hasMore = items.length < total;
  const groups = useMemo(() => groupByMonth(items), [items]);

  const clearFilters = () => {
    setQueryInput("");
    setQuery("");
    setMediaType("");
    setVerification("");
    setBackup("");
    setCamera({ make: "", model: "" });
    setDatePersisted({ kind: "any" });
  };

  return (
    <div>
      <PageHeader
        title="Library"
        description="A read-only view of everything archived — grouped by capture month, with full provenance on tap. Viewing only; nothing here modifies your files."
        actions={
          <div className="flex items-center gap-2">
            <div className="flex items-center rounded-md border border-zinc-700 bg-zinc-950 p-0.5">
              <SegBtn active={view === "grid"} icon={Squares2X2Icon} label="Grid" onClick={() => setViewPersisted("grid")} />
              <SegBtn active={view === "folders"} icon={FolderIcon} label="Folders" onClick={() => setViewPersisted("folders")} />
            </div>
            {warmRunning ? (
              <span className="flex items-center gap-1.5 rounded-md border border-blue-500/30 bg-blue-500/5 px-2.5 py-1.5 text-[12px] text-zinc-300 tabular-nums">
                <BoltIcon className="h-3.5 w-3.5 animate-pulse text-blue-400" />
                Generating thumbnails · {formatNumber(warm?.done ?? 0)} of {formatNumber(warm?.total ?? 0)}
              </span>
            ) : (
              <Button icon={BoltIcon} variant="ghost" onClick={() => void warmAll()} loading={startingWarm}>
                Pre-generate all thumbnails
              </Button>
            )}
            <Button
              icon={ArrowPathIcon}
              variant="secondary"
              onClick={() => {
                void load(1, true);
                void months.run({ silent: true });
                void years.run({ silent: true });
                void cameras.run({ silent: true });
              }}
              loading={loading && items.length > 0}
            >
              Refresh
            </Button>
          </div>
        }
      />

      {view === "folders" ? <FolderView fit={fitTiles} filtersActive={filtersActive} /> : null}

      {view === "grid" ? (
      <>
      <Card className="mb-5">
        <div className="space-y-3">
          {/* Search spans the width; the structured filters wrap in a tidy row below. */}
          <label className="block">
            <span className={FIELD_LABEL_CLASS}>Search</span>
            <div className="relative">
              <MagnifyingGlassIcon className="pointer-events-none absolute top-1/2 left-2.5 h-4 w-4 -translate-y-1/2 text-zinc-600" />
              <input
                value={queryInput}
                onChange={(e) => setQueryInput(e.target.value)}
                placeholder="Filename, path, camera, or lens…"
                className="w-full rounded-md border border-zinc-700 bg-zinc-950 py-1.5 pr-3 pl-8 text-[13px] text-zinc-200 outline-none focus:border-blue-500"
              />
            </div>
          </label>

          <div className="flex flex-wrap items-end gap-3">
            <label>
              <span className={FIELD_LABEL_CLASS}>Type</span>
              <select value={mediaType} onChange={(e) => setMediaType(e.target.value)} className={SELECT_CLASS}>
                {MEDIA_TYPES.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </select>
            </label>

            <CameraFilter cameras={cameras.data ?? []} value={camera} onChange={setCamera} />

            <DateFilter sel={dateSel} onChange={setDatePersisted} years={years.data ?? []} months={months.data ?? []} />

            <label>
              <span className={FIELD_LABEL_CLASS}>Verification</span>
              <select value={verification} onChange={(e) => setVerification(e.target.value)} className={SELECT_CLASS}>
                {VERIFICATION.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </select>
            </label>

            <label>
              <span className={FIELD_LABEL_CLASS}>Backup</span>
              <select value={backup} onChange={(e) => setBackup(e.target.value)} className={SELECT_CLASS}>
                {BACKUP.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </select>
            </label>

            {filtersActive ? (
              <Button size="sm" variant="ghost" icon={XMarkIcon} onClick={clearFilters}>
                Clear ({activeCount})
              </Button>
            ) : null}

            <div className="ml-auto">
              <Button
                size="sm"
                variant="ghost"
                icon={fitTiles ? ArrowsPointingInIcon : ArrowsPointingOutIcon}
                onClick={toggleFit}
                title={fitTiles ? "Crop tiles to square" : "Fit full image in tile"}
              >
                {fitTiles ? "Crop" : "Fit"}
              </Button>
            </div>
          </div>
        </div>
      </Card>

      {loading && items.length === 0 ? (
        <LoadingBlock label="Loading library…" />
      ) : items.length === 0 ? (
        <Card>
          <EmptyState
            icon={PhotoIcon}
            title={filtersActive ? "No matches" : "Nothing archived yet"}
            description={
              filtersActive
                ? `No assets match ${activeFilters.join(", ")}. Try clearing ${activeCount > 1 ? "them" : "it"}.`
                : "Import or adopt media and it will appear here, grouped by capture month."
            }
            action={
              filtersActive ? (
                <Button size="sm" variant="secondary" onClick={clearFilters}>
                  Clear filters
                </Button>
              ) : undefined
            }
          />
        </Card>
      ) : (
        <div className="space-y-6">
          <div className="text-[11px] text-zinc-500 tabular-nums">
            Showing {formatNumber(items.length)} of {formatNumber(total)}
          </div>
          {groups.map((g) => (
            <section key={g.key}>
              <h2 className="mb-2 text-[13px] font-semibold text-zinc-300">{g.label}</h2>
              <div className="grid grid-cols-2 gap-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-6">
                {g.items.map((asset) => (
                  <Thumb
                    key={asset.id}
                    asset={asset}
                    fit={fitTiles}
                    onClick={() => setSelectedId(asset.id)}
                    onContextMenu={(e) => {
                      e.preventDefault();
                      setMenu({
                        x: e.clientX,
                        y: e.clientY,
                        items: [
                          {
                            label: "Reveal in Finder",
                            icon: FolderOpenIcon,
                            disabled: !asset.hasArchiveCopy,
                            onClick: () => void revealAsset(asset.id),
                          },
                        ],
                      });
                    }}
                  />
                ))}
              </div>
            </section>
          ))}

          {hasMore ? (
            <div className="flex justify-center pt-1">
              <Button variant="secondary" onClick={() => void load(page + 1, false)} loading={loadingMore}>
                Load more ({formatNumber(total - items.length)} remaining)
              </Button>
            </div>
          ) : null}
        </div>
      )}
      </>
      ) : null}

      {view === "grid" && selectedId ? (
        <DetailDrawer
          assetId={selectedId}
          items={items}
          onClose={() => setSelectedId(null)}
          onNavigate={setSelectedId}
        />
      ) : null}

      <ContextMenu state={menu} onClose={() => setMenu(null)} />
    </div>
  );
}

/** A segmented-control button used by the Grid|Folders view switch. */
function SegBtn({
  active,
  icon: Icon,
  label,
  onClick,
}: {
  active: boolean;
  icon: React.ComponentType<{ className?: string }>;
  label: string;
  onClick: () => void;
}) {
  return (
    <button
      onClick={onClick}
      title={`${label} view`}
      className={`flex items-center gap-1.5 rounded px-2 py-1 text-[12px] font-medium transition ${
        active ? "bg-zinc-800 text-zinc-100" : "text-zinc-400 hover:text-zinc-200"
      }`}
    >
      <Icon className="h-4 w-4" />
      {label}
    </button>
  );
}

/**
 * CameraFilter — a dropdown of the distinct cameras in the library (make + model)
 * with per-camera counts, feeding an exact-match filter. The option value encodes
 * the make/model pair as JSON so arbitrary characters in a camera name round-trip
 * safely. Lens is intentionally NOT here (it lives in the text search) to keep the
 * dropdown from bloating.
 */
function CameraFilter({
  cameras,
  value,
  onChange,
}: {
  cameras: CameraCountDTO[];
  value: { make: string; model: string };
  onChange: (v: { make: string; model: string }) => void;
}) {
  const selected = value.make || value.model ? JSON.stringify([value.make, value.model]) : "";
  return (
    <label>
      <span className={FIELD_LABEL_CLASS}>Camera</span>
      <select
        value={selected}
        onChange={(e) => {
          if (!e.target.value) {
            onChange({ make: "", model: "" });
            return;
          }
          try {
            const [make, model] = JSON.parse(e.target.value) as [string, string];
            onChange({ make, model });
          } catch {
            onChange({ make: "", model: "" });
          }
        }}
        className={SELECT_CLASS}
      >
        <option value="">All cameras</option>
        {cameras.map((c) => {
          const v = JSON.stringify([c.make, c.model]);
          return (
            <option key={v} value={v}>
              {(c.label || "Unknown camera") + ` (${formatNumber(c.count)})`}
            </option>
          );
        })}
      </select>
    </label>
  );
}

/**
 * DateFilter — the compact date-range control that replaces the single-month
 * dropdown. The primary select offers Any time / This year / Last 12 months, a
 * group of specific years (with counts), and Custom range. Choosing a year reveals
 * a secondary month picker (All of <year> + that year's months) so the legacy
 * single-month behavior is still one click away; Custom range reveals two day
 * inputs. All modes map to the backend's effective-date bounds via dateSelToFilter.
 */
function DateFilter({
  sel,
  onChange,
  years,
  months,
}: {
  sel: DateSel;
  onChange: (s: DateSel) => void;
  years: YearCountDTO[];
  months: MonthCountDTO[];
}) {
  const activeYear =
    sel.kind === "year" ? sel.year : sel.kind === "month" ? sel.yearMonth.slice(0, 4) : "";
  const primaryValue =
    sel.kind === "year" || sel.kind === "month"
      ? `y:${activeYear}`
      : sel.kind === "custom"
        ? "custom"
        : sel.kind; // any | thisYear | last12

  const onPrimary = (v: string) => {
    if (v === "any" || v === "thisYear" || v === "last12") onChange({ kind: v } as DateSel);
    else if (v === "custom") onChange({ kind: "custom", from: "", to: "" });
    else if (v.startsWith("y:")) onChange({ kind: "year", year: v.slice(2) });
  };

  const monthsInYear = activeYear ? months.filter((m) => m.month.startsWith(`${activeYear}-`)) : [];

  return (
    <div className="flex flex-wrap items-end gap-3">
      <label>
        <span className={FIELD_LABEL_CLASS}>Date</span>
        <select value={primaryValue} onChange={(e) => onPrimary(e.target.value)} className={SELECT_CLASS}>
          <option value="any">Any time</option>
          <option value="thisYear">This year</option>
          <option value="last12">Last 12 months</option>
          {years.length > 0 ? (
            <optgroup label="Year">
              {years.map((y) => (
                <option key={y.year} value={`y:${y.year}`}>
                  {y.year} ({formatNumber(y.count)})
                </option>
              ))}
            </optgroup>
          ) : null}
          <option value="custom">Custom range…</option>
        </select>
      </label>

      {activeYear && monthsInYear.length > 0 ? (
        <label>
          <span className={FIELD_LABEL_CLASS}>Month</span>
          <select
            value={sel.kind === "month" ? sel.yearMonth : ""}
            onChange={(e) =>
              onChange(
                e.target.value
                  ? { kind: "month", yearMonth: e.target.value }
                  : { kind: "year", year: activeYear },
              )
            }
            className={SELECT_CLASS}
          >
            <option value="">All of {activeYear}</option>
            {monthsInYear.map((m) => (
              <option key={m.month} value={m.month}>
                {formatMonthLong(m.month)} ({formatNumber(m.count)})
              </option>
            ))}
          </select>
        </label>
      ) : null}

      {sel.kind === "custom" ? (
        <div className="flex items-end gap-2">
          <label>
            <span className={FIELD_LABEL_CLASS}>From</span>
            <input
              type="date"
              value={sel.from}
              max={sel.to || undefined}
              onChange={(e) => onChange({ kind: "custom", from: e.target.value, to: sel.to })}
              className={DATE_INPUT_CLASS}
            />
          </label>
          <label>
            <span className={FIELD_LABEL_CLASS}>To</span>
            <input
              type="date"
              value={sel.to}
              min={sel.from || undefined}
              onChange={(e) => onChange({ kind: "custom", from: sel.from, to: e.target.value })}
              className={DATE_INPUT_CLASS}
            />
          </label>
        </div>
      ) : null}
    </div>
  );
}

/** A single grid tile: lazy 512 thumbnail with badges and a placeholder fallback. */
function Thumb({
  asset,
  onClick,
  fit,
  onContextMenu,
}: {
  asset: BrowseAssetDTO;
  onClick: () => void;
  fit: boolean;
  onContextMenu?: (e: React.MouseEvent) => void;
}) {
  const [errored, setErrored] = useState(false);
  const isVideo = asset.mediaType === "video";
  return (
    <button
      onClick={onClick}
      onContextMenu={onContextMenu}
      title={asset.filename}
      className="group relative aspect-square overflow-hidden rounded-lg border border-zinc-800 bg-zinc-900 focus:ring-2 focus:ring-blue-500/60 focus:outline-none"
    >
      {errored ? (
        <PlaceholderTile isVideo={isVideo} />
      ) : (
        <img
          src={`/thumb/${asset.id}`}
          loading="lazy"
          alt={asset.filename}
          onError={() => setErrored(true)}
          className={`h-full w-full transition duration-200 group-hover:scale-[1.04] ${fit ? "object-contain" : "object-cover"}`}
        />
      )}

      {/* Live Photo badge (top-left). */}
      {asset.isLivePhotoPair ? (
        <span className="absolute top-1 left-1 rounded bg-black/60 px-1.5 py-0.5 text-[9px] font-semibold tracking-wide text-white backdrop-blur-sm">
          LIVE
        </span>
      ) : null}

      {/* Non-verified marker (top-right) — this is an integrity tool. */}
      {asset.verificationStatus !== "verified" ? (
        <span className="absolute top-1 right-1 h-2 w-2 rounded-full bg-amber-400 ring-2 ring-black/40" title={`Verification: ${asset.verificationStatus}`} />
      ) : null}

      {/* Video duration badge (bottom-right). */}
      {isVideo && asset.durationSeconds > 0 ? (
        <span className="absolute right-1 bottom-1 flex items-center gap-0.5 rounded bg-black/65 px-1 py-0.5 text-[9px] font-medium text-white backdrop-blur-sm">
          <FilmIcon className="h-2.5 w-2.5" />
          {formatDuration(asset.durationSeconds)}
        </span>
      ) : null}
    </button>
  );
}

/** Fallback tile shown when a thumbnail is missing or failed to render. */
function PlaceholderTile({ isVideo }: { isVideo: boolean }) {
  const Icon = isVideo ? FilmIcon : PhotoIcon;
  return (
    <div className="flex h-full w-full items-center justify-center bg-zinc-900">
      <Icon className="h-8 w-8 text-zinc-700" />
    </div>
  );
}

/* -------------------------------------------------------------------------- */
/* Detail drawer                                                              */
/* -------------------------------------------------------------------------- */

function DetailDrawer({
  assetId,
  items,
  onClose,
  onNavigate,
  reloadNonce,
}: {
  assetId: string;
  items: BrowseAssetDTO[];
  onClose: () => void;
  onNavigate: (id: string) => void;
  reloadNonce?: number;
}) {
  const toast = useToast();
  const detail = useAsyncData<AssetDetailDTO>(() => BrowserService.AssetDetail(assetId));

  useEffect(() => {
    void detail.run().catch((e) => toast.fromError(e, "Failed to load asset"));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [assetId, reloadNonce]);

  // Position within the CURRENTLY LOADED filtered grid list, for keyboard stepping.
  const index = useMemo(() => items.findIndex((it) => it.id === assetId), [items, assetId]);
  const prevId = index > 0 ? items[index - 1].id : null;
  const nextId = index >= 0 && index < items.length - 1 ? items[index + 1].id : null;

  // Keyboard: ArrowLeft/Right step within the grid list; Escape closes. Arrows are
  // ignored while a text input is focused so typing in a filter is never hijacked.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        onClose();
        return;
      }
      if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
      const el = document.activeElement as HTMLElement | null;
      const tag = el?.tagName;
      if (tag === "INPUT" || tag === "TEXTAREA" || el?.isContentEditable) return;
      if (e.key === "ArrowLeft" && prevId) {
        e.preventDefault();
        onNavigate(prevId);
      } else if (e.key === "ArrowRight" && nextId) {
        e.preventDefault();
        onNavigate(nextId);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose, onNavigate, prevId, nextId]);

  // Warm the adjacent 2048 previews so stepping feels instant.
  useEffect(() => {
    for (const id of [prevId, nextId]) {
      if (!id) continue;
      const img = new Image();
      img.src = `/thumb/${id}?s=2048`;
    }
  }, [prevId, nextId]);

  const d = detail.data;

  const revealArchive = async () => {
    if (!d) return;
    try {
      await BrowserService.RevealAsset(d.id, "archive");
    } catch (e) {
      toast.fromError(e, "Could not reveal in Finder");
    }
  };

  return (
    <div className="fixed inset-0 z-40 flex justify-end">
      <div className="absolute inset-0 bg-black/50 backdrop-blur-[1px]" onClick={onClose} />
      <aside className="relative flex h-full w-full max-w-5xl flex-col overflow-hidden border-l border-zinc-800 bg-zinc-950 shadow-2xl md:flex-row">
        {/* Preview pane (LEFT on wide, TOP when stacked) */}
        <div className="relative flex h-[42vh] w-full flex-none flex-col bg-zinc-900/40 md:h-full md:flex-1">
          {!d ? (
            <div className="flex flex-1 items-center justify-center">
              {detail.loading ? <LoadingBlock label="Loading preview…" /> : <PlaceholderTile isVideo={false} />}
            </div>
          ) : (
            <ZoomableImage key={d.id} assetId={d.id} alt={d.originalFilename} isVideo={d.mediaType === "video"} />
          )}

          {/* Prev/next arrows, overlaid on the preview. */}
          {prevId ? (
            <button
              onClick={() => onNavigate(prevId)}
              aria-label="Previous asset"
              title="Previous (←)"
              className="absolute top-1/2 left-2 z-10 -translate-y-1/2 rounded-full bg-black/50 p-2 text-zinc-200 backdrop-blur-sm transition hover:bg-black/70 hover:text-white"
            >
              <ChevronLeftIcon className="h-5 w-5" />
            </button>
          ) : null}
          {nextId ? (
            <button
              onClick={() => onNavigate(nextId)}
              aria-label="Next asset"
              title="Next (→)"
              className="absolute top-1/2 right-2 z-10 -translate-y-1/2 rounded-full bg-black/50 p-2 text-zinc-200 backdrop-blur-sm transition hover:bg-black/70 hover:text-white"
            >
              <ChevronRightIcon className="h-5 w-5" />
            </button>
          ) : null}
          {index >= 0 && items.length > 0 ? (
            <div className="absolute bottom-2 left-1/2 z-10 -translate-x-1/2 rounded-full bg-black/50 px-2.5 py-0.5 text-[11px] text-zinc-300 tabular-nums backdrop-blur-sm">
              {index + 1} / {items.length}
            </div>
          ) : null}
        </div>

        {/* Metadata pane (RIGHT on wide, BELOW when stacked) */}
        <div className="flex w-full min-w-0 flex-1 flex-col overflow-y-auto border-t border-zinc-800 bg-zinc-950 md:w-[360px] md:flex-none md:border-t-0 md:border-l">
          <header className="sticky top-0 z-10 flex items-center justify-between border-b border-zinc-800 bg-zinc-950/95 px-4 py-3 backdrop-blur">
            <div className="min-w-0">
              <div className="truncate text-[13px] font-semibold text-zinc-100" title={d?.originalFilename}>
                {d?.originalFilename ?? "Loading…"}
              </div>
              <div className="text-[11px] text-zinc-500">Provenance · read-only</div>
            </div>
            <button
              onClick={onClose}
              className="flex-none rounded-md p-1.5 text-zinc-400 hover:bg-zinc-800 hover:text-zinc-100"
              aria-label="Close"
            >
              <XMarkIcon className="h-5 w-5" />
            </button>
          </header>

          {detail.loading && !d ? (
            <LoadingBlock label="Loading asset…" />
          ) : !d ? (
            <div className="p-4 text-[13px] text-zinc-500">Could not load this asset.</div>
          ) : (
            <div className="space-y-5 p-4">
            {/* Reveal in Finder (archive copy) */}
            <Button
              size="sm"
              variant="secondary"
              icon={FolderOpenIcon}
              disabled={!d.currentArchivePath}
              title={d.currentArchivePath ? "Reveal the archived file in Finder" : "No archive copy to reveal"}
              onClick={() => void revealArchive()}
            >
              Reveal in Finder
            </Button>

            {/* Status badges */}
            <div className="flex flex-wrap gap-2">
              <StatusBadge status={d.verificationStatus} dot />
              <StatusBadge status={d.backupStatus} dot label={`Backup: ${badgeLabel(d.backupStatus)}`} />
              {d.isLivePhotoPair ? <StatusBadge status="live" tone="info" label="Live Photo" /> : null}
              {d.duplicateOf ? <StatusBadge status="duplicate" tone="warn" label="Duplicate" dot /> : null}
              {!d.currentArchivePath ? (
                <StatusBadge status="no-copy" tone="muted" label="No archive copy" />
              ) : null}
            </div>

            <Section title="File">
              <Row label="Filename" value={d.originalFilename} />
              <Row label="Type" value={mediaTypeLabel(d.mediaType)} />
              <Row label="Extension" value={d.originalExtension ? `.${d.originalExtension}` : "—"} />
              <Row label="Size" value={formatBytes(d.fileSize)} />
              {d.width > 0 || d.height > 0 ? <Row label="Dimensions" value={`${d.width} × ${d.height}`} /> : null}
              {d.durationSeconds > 0 ? <Row label="Duration" value={formatDuration(d.durationSeconds)} /> : null}
              <Row label="Captured" value={formatDate(d.captureDate)} />
              <Row label="Imported" value={formatDate(d.importDate)} />
            </Section>

            <Section title="Archive location">
              <PathRow value={d.currentArchivePath || "— (no physical copy)"} />
              <Row label="Original path" value={d.originalFullPath || "—"} mono />
            </Section>

            <Section title="Integrity hashes">
              <HashRow label="Quick" hash={d.quickHash} />
              <HashRow label="Full" hash={d.fullHash} />
            </Section>

            {hasCameraInfo(d) ? (
              <Section title="Camera & exposure">
                {d.cameraMake || d.cameraModel ? (
                  <Row label="Camera" value={[d.cameraMake, d.cameraModel].filter(Boolean).join(" ") || "—"} />
                ) : null}
                {d.lens ? <Row label="Lens" value={d.lens} /> : null}
                {d.iso > 0 ? <Row label="ISO" value={String(d.iso)} /> : null}
                {d.shutterSpeed ? <Row label="Shutter" value={d.shutterSpeed} /> : null}
                {d.aperture ? <Row label="Aperture" value={d.aperture} /> : null}
                {d.gpsLatitude != null && d.gpsLongitude != null ? (
                  <Row label="GPS" value={`${d.gpsLatitude.toFixed(5)}, ${d.gpsLongitude.toFixed(5)}`} mono />
                ) : null}
              </Section>
            ) : null}

            <Section title="Provenance">
              <Row label="Source" value={d.sourceLabel ? `${d.sourceLabel} (${sourceTypeLabel(d.sourceType)})` : "—"} />
              <Row label="Session" value={d.sessionDate ? formatDateOnly(d.sessionDate) : "—"} />
              {d.sessionId ? <Row label="Session ID" value={d.sessionId} mono /> : null}
            </Section>

            {(d.backupJobs ?? []).length > 0 ? (
              <Section title={`Backups (${(d.backupJobs ?? []).length})`}>
                <div className="space-y-1.5">
                  {(d.backupJobs ?? []).map((j, i) => (
                    <div key={i} className="flex items-center justify-between gap-2 text-[11px]">
                      <div className="min-w-0">
                        <span className="text-zinc-300">{j.plugin || "—"}</span>
                        <span className="ml-1 truncate font-mono text-zinc-600" title={j.destination}>
                          → {j.destination || "—"}
                        </span>
                      </div>
                      <div className="flex flex-none items-center gap-2">
                        {j.completedAt ? (
                          <span className="text-zinc-600 tabular-nums">{formatDateOnly(j.completedAt)}</span>
                        ) : null}
                        <StatusBadge status={j.status} />
                      </div>
                    </div>
                  ))}
                </div>
              </Section>
            ) : null}

            {(d.duplicateOf || (d.duplicates ?? []).length > 0 || d.livePhotoPartner) && (
              <Section title="Relationships">
                {d.duplicateOf ? (
                  <RefLink label="Duplicate of" refItem={d.duplicateOf} onClick={() => onNavigate(d.duplicateOf!.id)} />
                ) : null}
                {d.livePhotoPartner ? (
                  <RefLink
                    label="Live Photo partner"
                    refItem={d.livePhotoPartner}
                    onClick={() => onNavigate(d.livePhotoPartner!.id)}
                  />
                ) : null}
                {(d.duplicates ?? []).map((r) => (
                  <RefLink key={r.id} label="Duplicated by" refItem={r} onClick={() => onNavigate(r.id)} />
                ))}
              </Section>
            )}
            </div>
          )}
        </div>
      </aside>
    </div>
  );
}

const MIN_SCALE = 1;
const MAX_SCALE = 8;
const clampScale = (s: number) => Math.max(MIN_SCALE, Math.min(MAX_SCALE, s));

interface ViewState {
  scale: number;
  tx: number;
  ty: number;
}
const FIT_VIEW: ViewState = { scale: 1, tx: 0, ty: 0 };

/**
 * ZoomableImage renders an asset's preview with pan/zoom, keyed by asset so it
 * remounts (and resets zoom) on navigation.
 *
 * Progressive loading: the 512 grid thumb (already warm in the browser cache from
 * the grid) paints IMMEDIATELY and stays fully interactive; the 2048 preview is
 * fetched offscreen and swapped into the same layout box on load, preserving the
 * current zoom/pan (transforms live on the container, not the <img> src). If the
 * 2048 fails we silently stay on the 512 (console warning only). Zoom is capped
 * at MAX_SCALE; beyond the 2048's native pixels the preview upscales — we never
 * load original bytes (originals may be huge RAWs the browser cannot decode).
 */
function ZoomableImage({ assetId, alt, isVideo }: { assetId: string; alt: string; isVideo: boolean }) {
  const containerRef = useRef<HTMLDivElement>(null);
  const [view, setView] = useState<ViewState>(FIT_VIEW);
  const viewRef = useRef(view);
  useEffect(() => {
    viewRef.current = view;
  }, [view]);

  // Start on the 512 (instant from cache); swap to 2048 once it's confirmed loadable.
  const [src, setSrc] = useState(`/thumb/${assetId}`);
  const [hiResLoading, setHiResLoading] = useState(true);
  const [gridError, setGridError] = useState(false);
  const natural = useRef({ w: 0, h: 0 });

  useEffect(() => {
    setHiResLoading(true);
    const img = new Image();
    img.onload = () => {
      natural.current = { w: img.naturalWidth, h: img.naturalHeight };
      setSrc(`/thumb/${assetId}?s=2048`);
      setHiResLoading(false);
    };
    img.onerror = () => {
      // 2048 unavailable — keep the 512 showing, no user-facing error.
      console.warn(`library: 2048 preview failed for ${assetId}; staying on 512`);
      setHiResLoading(false);
    };
    img.src = `/thumb/${assetId}?s=2048`;
    return () => {
      img.onload = null;
      img.onerror = null;
    };
  }, [assetId]);

  // Zoom toward a screen point (clientX/clientY), keeping that point fixed.
  const zoomAt = useCallback((nextScale: number, screenX: number, screenY: number) => {
    const el = containerRef.current;
    if (!el) return;
    const rect = el.getBoundingClientRect();
    const ns = clampScale(nextScale);
    const { scale, tx, ty } = viewRef.current;
    if (ns === scale) return;
    if (ns === 1) {
      setView(FIT_VIEW);
      return;
    }
    const cx = screenX - rect.left - rect.width / 2;
    const cy = screenY - rect.top - rect.height / 2;
    const ratio = ns / scale;
    setView({ scale: ns, tx: cx - (cx - tx) * ratio, ty: cy - (cy - ty) * ratio });
  }, []);

  const zoomAtCenter = useCallback(
    (nextScale: number) => {
      const el = containerRef.current;
      if (!el) return;
      const rect = el.getBoundingClientRect();
      zoomAt(nextScale, rect.left + rect.width / 2, rect.top + rect.height / 2);
    },
    [zoomAt],
  );

  // "100%" — one 2048 pixel per screen pixel (clamped to fit .. MAX_SCALE).
  const oneToOneScale = useCallback(() => {
    const el = containerRef.current;
    if (!el || !natural.current.w || !natural.current.h) return MIN_SCALE;
    const containFactor = Math.min(el.clientWidth / natural.current.w, el.clientHeight / natural.current.h);
    if (containFactor <= 0) return MIN_SCALE;
    return clampScale(1 / containFactor);
  }, []);

  // Native wheel listener (non-passive) so we can preventDefault the page scroll.
  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const onWheel = (e: WheelEvent) => {
      e.preventDefault();
      const factor = Math.exp(-e.deltaY * 0.0015);
      zoomAt(viewRef.current.scale * factor, e.clientX, e.clientY);
    };
    el.addEventListener("wheel", onWheel, { passive: false });
    return () => el.removeEventListener("wheel", onWheel);
  }, [zoomAt]);

  // Drag-to-pan (pointer events) while zoomed in.
  const drag = useRef<{ x: number; y: number; tx: number; ty: number } | null>(null);
  const [dragging, setDragging] = useState(false);
  const onPointerDown = (e: React.PointerEvent) => {
    if (viewRef.current.scale <= 1) return;
    (e.target as Element).setPointerCapture?.(e.pointerId);
    drag.current = { x: e.clientX, y: e.clientY, tx: viewRef.current.tx, ty: viewRef.current.ty };
    setDragging(true);
  };
  const onPointerMove = (e: React.PointerEvent) => {
    if (!drag.current) return;
    setView((v) => ({ ...v, tx: drag.current!.tx + (e.clientX - drag.current!.x), ty: drag.current!.ty + (e.clientY - drag.current!.y) }));
  };
  const endDrag = (e: React.PointerEvent) => {
    if (!drag.current) return;
    (e.target as Element).releasePointerCapture?.(e.pointerId);
    drag.current = null;
    setDragging(false);
  };

  // Double-click toggles Fit <-> 100%.
  const onDoubleClick = () => {
    if (viewRef.current.scale > 1) setView(FIT_VIEW);
    else zoomAtCenter(oneToOneScale());
  };

  const zoomed = view.scale > 1;

  return (
    <div
      ref={containerRef}
      onDoubleClick={onDoubleClick}
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={endDrag}
      onPointerCancel={endDrag}
      className="relative flex min-h-0 flex-1 touch-none items-center justify-center overflow-hidden select-none"
      style={{ cursor: zoomed ? (dragging ? "grabbing" : "grab") : "default" }}
    >
      {gridError ? (
        <PlaceholderTile isVideo={isVideo} />
      ) : (
        <div
          className="absolute inset-0 flex items-center justify-center will-change-transform"
          style={{ transform: `translate(${view.tx}px, ${view.ty}px) scale(${view.scale})`, transformOrigin: "center center" }}
        >
          <img
            src={src}
            alt={alt}
            draggable={false}
            onLoad={(e) => {
              natural.current = { w: e.currentTarget.naturalWidth, h: e.currentTarget.naturalHeight };
            }}
            onError={() => {
              // Only the 512 reaches the visible <img> via onError (the 2048 is
              // swapped in only after a successful offscreen load).
              setGridError(true);
            }}
            className="max-h-full max-w-full object-contain"
          />
        </div>
      )}

      {/* Loading indicator while the 2048 is still fetching (512 remains visible). */}
      {hiResLoading && !gridError ? (
        <div className="absolute top-2 right-2 z-10 rounded-full bg-black/50 px-2 py-0.5 text-[10px] text-zinc-300 backdrop-blur-sm">
          Loading HD…
        </div>
      ) : null}

      {/* Zoom controls */}
      {!gridError ? (
        <div className="absolute top-2 left-2 z-10 flex items-center gap-0.5 rounded-lg bg-black/55 p-1 backdrop-blur-sm">
          <ZoomBtn title="Zoom out (scroll)" onClick={() => zoomAtCenter(view.scale / 1.4)}>
            <MagnifyingGlassMinusIcon className="h-4 w-4" />
          </ZoomBtn>
          <ZoomBtn title="Zoom in (scroll)" onClick={() => zoomAtCenter(view.scale * 1.4)}>
            <MagnifyingGlassPlusIcon className="h-4 w-4" />
          </ZoomBtn>
          <ZoomBtn title="Fit (double-click)" onClick={() => setView(FIT_VIEW)} active={!zoomed}>
            <ArrowsPointingInIcon className="h-4 w-4" />
          </ZoomBtn>
          <button
            title="Actual pixels (double-click)"
            onClick={() => zoomAtCenter(oneToOneScale())}
            className="rounded px-1.5 py-1 text-[10px] font-semibold text-zinc-200 hover:bg-white/10"
          >
            100%
          </button>
        </div>
      ) : null}
    </div>
  );
}

function ZoomBtn({
  title,
  onClick,
  active = false,
  children,
}: {
  title: string;
  onClick: () => void;
  active?: boolean;
  children: React.ReactNode;
}) {
  return (
    <button
      title={title}
      onClick={onClick}
      className={`rounded p-1 transition hover:bg-white/10 ${active ? "text-blue-400" : "text-zinc-200"}`}
    >
      {children}
    </button>
  );
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div>
      <h3 className="mb-1.5 text-[11px] font-semibold tracking-wide text-zinc-500 uppercase">{title}</h3>
      <div className="space-y-1 rounded-lg border border-zinc-800 bg-zinc-900/40 p-3">{children}</div>
    </div>
  );
}

function Row({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="flex items-start justify-between gap-3 text-[12px]">
      <span className="flex-none text-zinc-500">{label}</span>
      <span className={`min-w-0 truncate text-right text-zinc-300 ${mono ? "font-mono text-[11px]" : ""}`} title={value}>
        {value}
      </span>
    </div>
  );
}

function PathRow({ value }: { value: string }) {
  return (
    <div className="selectable font-mono text-[11px] break-all text-zinc-400" title={value}>
      {value}
    </div>
  );
}

function RefLink({ label, refItem, onClick }: { label: string; refItem: AssetRefDTO; onClick: () => void }) {
  return (
    <button
      onClick={onClick}
      className="flex w-full items-center justify-between gap-2 rounded-md px-1 py-1 text-left text-[12px] transition hover:bg-zinc-800/60"
    >
      <span className="flex-none text-zinc-500">{label}</span>
      <span className="min-w-0 truncate text-right text-blue-400 hover:text-blue-300" title={refItem.filename}>
        {refItem.filename} ›
      </span>
    </button>
  );
}

/** Click-to-copy hash row (mirrors the Duplicate Manager treatment). */
function HashRow({ label, hash }: { label: string; hash: string }) {
  const toast = useToast();
  const [copied, setCopied] = useState(false);
  if (!hash) {
    return (
      <div className="flex items-center gap-2 text-[11px] text-zinc-600">
        <span className="w-9">{label}</span>
        <span>not computed</span>
      </div>
    );
  }
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(hash);
      setCopied(true);
      setTimeout(() => setCopied(false), 1200);
    } catch {
      toast.warn("Could not copy to clipboard");
    }
  };
  return (
    <button
      onClick={() => void copy()}
      className="group flex w-full items-center gap-2 text-left text-[11px] text-zinc-500 transition hover:text-zinc-300"
      title={`${hash} — click to copy`}
    >
      <span className="w-9 flex-none text-zinc-600">{label}</span>
      <span className="min-w-0 flex-1 truncate font-mono">{hash}</span>
      {copied ? (
        <CheckIcon className="h-3.5 w-3.5 flex-none text-emerald-400" />
      ) : (
        <ClipboardDocumentIcon className="h-3.5 w-3.5 flex-none opacity-0 transition group-hover:opacity-100" />
      )}
    </button>
  );
}

/* -------------------------------------------------------------------------- */
/* Helpers                                                                     */
/* -------------------------------------------------------------------------- */

interface MonthGroup {
  key: string;
  label: string;
  items: BrowseAssetDTO[];
}

/** Buckets capture-date-sorted items into consecutive month sections. */
function groupByMonth(items: BrowseAssetDTO[]): MonthGroup[] {
  const groups: MonthGroup[] = [];
  let cur: MonthGroup | null = null;
  for (const it of items) {
    const key = monthKey(it.captureDate);
    if (!cur || cur.key !== key) {
      cur = { key, label: key === "undated" ? "Undated" : formatMonthLong(key), items: [] };
      groups.push(cur);
    }
    cur.items.push(it);
  }
  return groups;
}

function monthKey(value: string | null | undefined): string {
  if (!value) return "undated";
  const d = new Date(value);
  if (isNaN(d.getTime()) || d.getUTCFullYear() <= 1) return "undated";
  return `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, "0")}`;
}

const MEDIA_LABEL: Record<string, string> = {
  photo: "Photo",
  raw_photo: "RAW photo",
  video: "Video",
  live_photo_pair: "Live Photo",
};
function mediaTypeLabel(v: string): string {
  return MEDIA_LABEL[v] ?? v;
}

const SOURCE_LABEL: Record<string, string> = {
  sd_card: "SD card",
  usb_ssd: "USB SSD",
  external_hdd: "External HDD",
  internal_folder: "Internal folder",
  nas_folder: "NAS folder",
  smb_share: "SMB share",
};
function sourceTypeLabel(v: string): string {
  return SOURCE_LABEL[v] ?? v ?? "—";
}

function badgeLabel(v: string): string {
  return v ? v.charAt(0).toUpperCase() + v.slice(1) : "—";
}

function hasCameraInfo(d: AssetDetailDTO): boolean {
  return !!(
    d.cameraMake ||
    d.cameraModel ||
    d.lens ||
    d.iso > 0 ||
    d.shutterSpeed ||
    d.aperture ||
    (d.gpsLatitude != null && d.gpsLongitude != null)
  );
}

/* -------------------------------------------------------------------------- */
/* Folder view                                                                */
/* -------------------------------------------------------------------------- */

interface MenuItem {
  label: string;
  icon: React.ComponentType<{ className?: string }>;
  onClick: () => void;
  disabled?: boolean;
}
type ContextMenuState = { x: number; y: number; items: MenuItem[] } | null;

/**
 * ContextMenu — a custom fixed-position menu (the webview has no native context
 * menus). Closes on click-away and Escape; the first item is focused on open and
 * ArrowUp/Down move focus, so it is keyboard accessible.
 */
function ContextMenu({ state, onClose }: { state: ContextMenuState; onClose: () => void }) {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!state) return;
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) onClose();
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("mousedown", onDown);
    window.addEventListener("keydown", onKey, true);
    const raf = requestAnimationFrame(() => {
      ref.current?.querySelector<HTMLButtonElement>("button:not([disabled])")?.focus();
    });
    return () => {
      window.removeEventListener("mousedown", onDown);
      window.removeEventListener("keydown", onKey, true);
      cancelAnimationFrame(raf);
    };
  }, [state, onClose]);

  if (!state) return null;

  // Clamp to the viewport so the menu never spills off-screen.
  const estHeight = state.items.length * 34 + 8;
  const top = Math.min(state.y, Math.max(8, window.innerHeight - estHeight));
  const left = Math.min(state.x, Math.max(8, window.innerWidth - 208));

  const moveFocus = (e: React.KeyboardEvent, dir: number) => {
    if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
    e.preventDefault();
    const btns = Array.from(ref.current?.querySelectorAll<HTMLButtonElement>("button:not([disabled])") ?? []);
    if (btns.length === 0) return;
    const idx = btns.indexOf(document.activeElement as HTMLButtonElement);
    const next = (idx + dir + btns.length) % btns.length;
    btns[next].focus();
  };

  return (
    <div
      ref={ref}
      role="menu"
      style={{ position: "fixed", top, left }}
      onKeyDown={(e) => moveFocus(e, e.key === "ArrowUp" ? -1 : 1)}
      className="z-50 min-w-[200px] overflow-hidden rounded-lg border border-zinc-700 bg-zinc-900 py-1 shadow-2xl"
    >
      {state.items.map((it, i) => (
        <button
          key={i}
          role="menuitem"
          disabled={it.disabled}
          onClick={() => {
            onClose();
            it.onClick();
          }}
          className="flex w-full items-center gap-2 px-3 py-1.5 text-left text-[12px] text-zinc-200 hover:bg-zinc-800 focus:bg-zinc-800 focus:outline-none disabled:cursor-not-allowed disabled:text-zinc-600 disabled:hover:bg-transparent"
        >
          <it.icon className="h-4 w-4 flex-none text-zinc-400" />
          {it.label}
        </button>
      ))}
    </div>
  );
}

const FOLDER_PAGE_SIZE = 90;

/* -------------------------------------------------------------------------- */
/* Folder sorting                                                              */
/* -------------------------------------------------------------------------- */

type FolderSortKey = "name" | "date" | "items";
type SortDir = "asc" | "desc";
interface FolderSort {
  key: FolderSortKey;
  dir: SortDir;
}

// The direction a column resets to when it first becomes the active sort:
// name reads best A→Z, date and item-count newest/most first.
const FOLDER_SORT_DEFAULTS: Record<FolderSortKey, SortDir> = { name: "asc", date: "desc", items: "desc" };
const FOLDER_SORT_KEY = "paim.library.folderSort";

function loadFolderSort(): FolderSort {
  const raw = localStorage.getItem(FOLDER_SORT_KEY);
  if (raw) {
    const [key, dir] = raw.split(":");
    if ((key === "name" || key === "date" || key === "items") && (dir === "asc" || dir === "desc")) {
      return { key, dir };
    }
  }
  return { key: "date", dir: "desc" };
}
function saveFolderSort(s: FolderSort): void {
  localStorage.setItem(FOLDER_SORT_KEY, `${s.key}:${s.dir}`);
}

const naturalName = (a: string, b: string) =>
  a.localeCompare(b, undefined, { numeric: true, sensitivity: "base" });

/**
 * sortFolders orders a fully-loaded folder level client-side. name uses a
 * natural/numeric locale compare; date uses newestCapture with NULLs always last
 * (regardless of direction); items uses the recursive asset count. Ties fall back
 * to natural name so the order is stable.
 */
function sortFolders(folders: FolderEntryDTO[], sort: FolderSort): FolderEntryDTO[] {
  const dir = sort.dir === "asc" ? 1 : -1;
  return [...folders].sort((a, b) => {
    let cmp = 0;
    if (sort.key === "name") {
      cmp = naturalName(a.name, b.name);
    } else if (sort.key === "items") {
      cmp = a.assetCount - b.assetCount;
    } else {
      const at = a.newestCapture ? Date.parse(a.newestCapture) : NaN;
      const bt = b.newestCapture ? Date.parse(b.newestCapture) : NaN;
      const aNull = Number.isNaN(at);
      const bNull = Number.isNaN(bt);
      if (aNull && bNull) cmp = 0;
      else if (aNull) return 1; // nulls last, regardless of direction
      else if (bNull) return -1;
      else cmp = at - bt;
    }
    if (cmp === 0) return naturalName(a.name, b.name);
    return cmp * dir;
  });
}

/** One clickable sort-header column, mirroring the DataTable arrow convention. */
function SortColBtn({
  label,
  active,
  dir,
  onClick,
  className = "",
}: {
  label: string;
  active: boolean;
  dir: SortDir;
  onClick: () => void;
  className?: string;
}) {
  return (
    <button
      onClick={onClick}
      className={`flex items-center gap-1 text-[11px] font-medium tracking-wide uppercase transition ${
        active ? "text-zinc-200" : "text-zinc-500 hover:text-zinc-300"
      } ${className}`}
    >
      {label}
      {active ? (
        dir === "asc" ? (
          <ChevronUpIcon className="h-3.5 w-3.5 text-blue-400" />
        ) : (
          <ChevronDownIcon className="h-3.5 w-3.5 text-blue-400" />
        )
      ) : (
        <ChevronUpDownIcon className="h-3.5 w-3.5 text-zinc-600" />
      )}
    </button>
  );
}

/**
 * FolderSortHeader — the Name | Date | Items header row. Its columns align with
 * the folder rows below (cover + icon spacers on the left, chevron spacer on the
 * right). Name and Date order BOTH the folder rows and the folder's assets; Items
 * reorders folders only.
 */
function FolderSortHeader({ sort, onSort }: { sort: FolderSort; onSort: (key: FolderSortKey) => void }) {
  return (
    <div className="flex w-full items-center gap-3 border-b border-zinc-800 bg-zinc-900/40 px-3 py-2">
      <div className="h-9 w-9 flex-none" />
      <div className="h-4 w-4 flex-none" />
      <SortColBtn
        className="min-w-0 flex-1 justify-start"
        label="Name"
        active={sort.key === "name"}
        dir={sort.dir}
        onClick={() => onSort("name")}
      />
      <SortColBtn
        className="w-24 flex-none justify-end"
        label="Date"
        active={sort.key === "date"}
        dir={sort.dir}
        onClick={() => onSort("date")}
      />
      <SortColBtn
        className="w-16 flex-none justify-end"
        label="Items"
        active={sort.key === "items"}
        dir={sort.dir}
        onClick={() => onSort("items")}
      />
      <div className="h-4 w-4 flex-none" />
    </div>
  );
}

/**
 * FolderView — the archive tree, driven by BrowserService.ListFolder. Breadcrumb
 * navigation, a sortable Name | Date | Items header, folder rows (cover thumb,
 * name, newest date, count) that drill in, and the folder's own assets as rows
 * below. A custom right-click menu offers Rename… / Reveal in Finder on
 * date-event folders and Reveal in Finder on assets.
 *
 * Sorting: the header persists to localStorage (paim.library.folderSort). Folders
 * are fully loaded so they sort CLIENT-side. Assets are server-paginated, so Name
 * and Date are threaded to ListFolder and reset pagination; Items reorders folders
 * only and leaves the asset order (and its server sort) untouched.
 */
function FolderView({ fit, filtersActive }: { fit: boolean; filtersActive: boolean }) {
  const toast = useToast();
  const [relDir, setRelDir] = useState("");
  const [listing, setListing] = useState<FolderListingDTO | null>(null);
  const [assets, setAssets] = useState<BrowseAssetDTO[]>([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(0);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [menu, setMenu] = useState<ContextMenuState>(null);
  const [renameTarget, setRenameTarget] = useState<{ relDir: string; label: string } | null>(null);
  const [drawerNonce, setDrawerNonce] = useState(0);

  // Header sort. folderSort drives the (client-side) folder ordering; assetSort is
  // the server sort for the folder's own assets and follows folderSort whenever the
  // active column is Name or Date. Selecting Items reorders folders only and leaves
  // the asset order (and its server sort) as it was.
  const [folderSort, setFolderSort] = useState<FolderSort>(() => loadFolderSort());
  const [assetSort, setAssetSort] = useState<{ by: "name" | "date"; dir: SortDir }>(() => {
    const s = loadFolderSort();
    return s.key === "items" ? { by: "date", dir: "desc" } : { by: s.key, dir: s.dir };
  });
  useEffect(() => {
    if (folderSort.key !== "items") setAssetSort({ by: folderSort.key, dir: folderSort.dir });
  }, [folderSort]);

  const cycleSort = useCallback((key: FolderSortKey) => {
    setFolderSort((cur) => {
      const next: FolderSort =
        cur.key === key
          ? { key, dir: cur.dir === "asc" ? "desc" : "asc" }
          : { key, dir: FOLDER_SORT_DEFAULTS[key] };
      saveFolderSort(next);
      return next;
    });
  }, []);

  const load = useCallback(
    async (dir: string, pageNum: number, reset: boolean) => {
      if (reset) setLoading(true);
      else setLoadingMore(true);
      try {
        const res = await BrowserService.ListFolder(dir, pageNum, FOLDER_PAGE_SIZE, assetSort.by, assetSort.dir);
        setListing(res);
        setTotal(res.assets?.total ?? 0);
        const incoming = res.assets?.items ?? [];
        setAssets((prev) => (reset ? incoming : [...prev, ...incoming]));
        setPage(pageNum);
      } catch (e) {
        toast.fromError(e, "Failed to load folder");
      } finally {
        setLoading(false);
        setLoadingMore(false);
      }
    },
    [toast, assetSort],
  );

  // Reload page 1 on directory change OR asset-sort change (load's identity tracks
  // assetSort), which also resets the "Load more" pagination.
  useEffect(() => {
    void load(relDir, 1, true);
  }, [relDir, load]);

  const sortedFolders = useMemo(
    () => sortFolders(listing?.subfolders ?? [], folderSort),
    [listing, folderSort],
  );

  const revealAsset = async (id: string) => {
    try {
      await BrowserService.RevealAsset(id, "archive");
    } catch (e) {
      toast.fromError(e, "Could not reveal in Finder");
    }
  };
  const revealFolder = async (dir: string) => {
    try {
      await BrowserService.RevealFolder(dir);
    } catch (e) {
      toast.fromError(e, "Could not reveal folder in Finder");
    }
  };

  const doRename = async (dir: string, newLabel: string) => {
    try {
      await BrowserService.RenameEventFolder(dir, newLabel);
      toast.success("Folder renamed");
      setRenameTarget(null);
      // If we renamed the folder we're currently inside, follow it to its new path;
      // otherwise just reload the current listing (a subfolder row changed).
      if (dir === relDir) {
        const y = dir.slice(0, dir.lastIndexOf("/"));
        const datePart = dir.slice(dir.lastIndexOf("/") + 1, dir.lastIndexOf("/") + 11);
        const nb = newLabel.trim() ? `${datePart} ${newLabel.trim()}` : datePart;
        setRelDir(y ? `${y}/${nb}` : nb);
      } else {
        await load(relDir, 1, true);
      }
      setDrawerNonce((n) => n + 1); // refresh an open drawer's provenance paths
    } catch (e) {
      toast.fromError(e, "Could not rename folder");
    }
  };

  const openFolderMenu = (e: React.MouseEvent, entry: FolderEntryDTO) => {
    e.preventDefault();
    const items: MenuItem[] = [];
    if (entry.isDateFolder) {
      items.push({
        label: "Rename…",
        icon: PencilSquareIcon,
        onClick: () => setRenameTarget({ relDir: entry.relPath, label: labelOfDateFolder(entry.name) }),
      });
    }
    items.push({ label: "Reveal in Finder", icon: FolderOpenIcon, onClick: () => void revealFolder(entry.relPath) });
    setMenu({ x: e.clientX, y: e.clientY, items });
  };

  const openAssetMenu = (e: React.MouseEvent, asset: BrowseAssetDTO) => {
    e.preventDefault();
    setMenu({
      x: e.clientX,
      y: e.clientY,
      items: [
        {
          label: "Reveal in Finder",
          icon: FolderOpenIcon,
          disabled: !asset.hasArchiveCopy,
          onClick: () => void revealAsset(asset.id),
        },
      ],
    });
  };

  const crumbs = breadcrumbs(relDir);
  const hasMore = assets.length < total;

  return (
    <div>
      {/* Breadcrumbs + (optional) rename of the current date folder. */}
      <div className="mb-4 flex items-center justify-between gap-3">
        <nav className="flex flex-wrap items-center gap-1 text-[13px]">
          {crumbs.map((c, i) => (
            <span key={c.path} className="flex items-center gap-1">
              {i > 0 ? <ChevronRightIcon className="h-3.5 w-3.5 text-zinc-600" /> : null}
              {i === crumbs.length - 1 ? (
                <span className="font-medium text-zinc-200">{c.label}</span>
              ) : (
                <button className="text-blue-400 hover:text-blue-300" onClick={() => setRelDir(c.path)}>
                  {c.label}
                </button>
              )}
            </span>
          ))}
        </nav>
        {listing?.isDateFolder ? (
          <Button
            size="sm"
            variant="ghost"
            icon={PencilSquareIcon}
            onClick={() => setRenameTarget({ relDir, label: listing.label })}
          >
            Rename event
          </Button>
        ) : null}
      </div>

      {/* Folder browsing is structural, so the grid's search/type/status/camera/
          date filters do not apply here. They are kept in state and re-applied on
          the switch back to Grid; this note explains the difference. */}
      {filtersActive ? (
        <div className="mb-4 flex items-center gap-2 rounded-md border border-zinc-800 bg-zinc-900/50 px-3 py-2 text-[12px] text-zinc-400">
          <FunnelIcon className="h-4 w-4 flex-none text-zinc-500" />
          Filters apply to grid view — folder browsing shows the full archive tree.
        </div>
      ) : null}

      {loading && !listing ? (
        <LoadingBlock label="Loading folder…" />
      ) : (
        <div className="space-y-5">
          {/* Sortable header + subfolders. The header shows whenever there is any
              content, since Name/Date also govern the asset order below. */}
          {sortedFolders.length > 0 || assets.length > 0 ? (
            <div className="overflow-hidden rounded-lg border border-zinc-800">
              <FolderSortHeader sort={folderSort} onSort={cycleSort} />
              {sortedFolders.map((f) => (
                <button
                  key={f.relPath}
                  onClick={() => setRelDir(f.relPath)}
                  onContextMenu={(e) => openFolderMenu(e, f)}
                  className="flex w-full items-center gap-3 border-b border-zinc-800 px-3 py-2 text-left last:border-b-0 hover:bg-zinc-900/60"
                >
                  <FolderCover coverId={f.coverAssetId} />
                  <FolderIcon className="h-4 w-4 flex-none text-zinc-500" />
                  <span className="min-w-0 flex-1 truncate text-[13px] text-zinc-200">{f.name}</span>
                  <span className="w-24 flex-none text-right text-[11px] text-zinc-500 tabular-nums">
                    {f.newestCapture ? formatDateOnly(f.newestCapture) : "—"}
                  </span>
                  <span className="w-16 flex-none text-right text-[11px] text-zinc-500 tabular-nums">
                    {formatNumber(f.assetCount)}
                  </span>
                  <ChevronRightIcon className="h-4 w-4 flex-none text-zinc-600" />
                </button>
              ))}
            </div>
          ) : null}

          {/* Assets directly in this folder */}
          {assets.length > 0 ? (
            <div className="overflow-hidden rounded-lg border border-zinc-800">
              {assets.map((a) => (
                <button
                  key={a.id}
                  onClick={() => setSelectedId(a.id)}
                  onContextMenu={(e) => openAssetMenu(e, a)}
                  className="flex w-full items-center gap-3 border-b border-zinc-800 px-3 py-2 text-left last:border-b-0 hover:bg-zinc-900/60"
                >
                  <AssetRowThumb asset={a} fit={fit} />
                  <span className="min-w-0 flex-1 truncate text-[13px] text-zinc-200">{a.filename}</span>
                  <div className="flex flex-none items-center gap-2">
                    {a.isLivePhotoPair ? <StatusBadge status="live" tone="info" label="Live" /> : null}
                    {a.verificationStatus !== "verified" ? <StatusBadge status={a.verificationStatus} dot /> : null}
                    <span className="w-24 text-right text-[11px] text-zinc-500 tabular-nums">
                      {a.captureDate ? formatDateOnly(a.captureDate) : "—"}
                    </span>
                  </div>
                </button>
              ))}
            </div>
          ) : null}

          {(listing?.subfolders ?? []).length === 0 && assets.length === 0 ? (
            <Card>
              <EmptyState icon={FolderIcon} title="Empty folder" description="This folder has no archived items." />
            </Card>
          ) : null}

          {hasMore ? (
            <div className="flex justify-center pt-1">
              <Button variant="secondary" onClick={() => void load(relDir, page + 1, false)} loading={loadingMore}>
                Load more ({formatNumber(total - assets.length)} remaining)
              </Button>
            </div>
          ) : null}
        </div>
      )}

      {selectedId ? (
        <DetailDrawer
          assetId={selectedId}
          items={assets}
          reloadNonce={drawerNonce}
          onClose={() => setSelectedId(null)}
          onNavigate={setSelectedId}
        />
      ) : null}

      <ContextMenu state={menu} onClose={() => setMenu(null)} />

      {renameTarget ? (
        <RenameFolderDialog
          relDir={renameTarget.relDir}
          initialLabel={renameTarget.label}
          onCancel={() => setRenameTarget(null)}
          onSubmit={(label) => void doRename(renameTarget.relDir, label)}
        />
      ) : null}
    </div>
  );
}

/**
 * Small cover thumbnail for a folder row. Shows a PhotoIcon placeholder (matching
 * the grid tile treatment) when there is no cover or the thumbnail fails to render
 * — e.g. a RAW cover whose thumbnail could not be generated — rather than a blank
 * box, so the row never looks empty.
 */
function FolderCover({ coverId }: { coverId: string }) {
  const [errored, setErrored] = useState(false);
  if (!coverId || errored) {
    return (
      <div className="flex h-9 w-9 flex-none items-center justify-center rounded bg-zinc-900">
        <PhotoIcon className="h-4 w-4 text-zinc-700" />
      </div>
    );
  }
  return (
    <img
      src={`/thumb/${coverId}`}
      loading="lazy"
      alt=""
      onError={() => setErrored(true)}
      className="h-9 w-9 flex-none rounded object-cover"
    />
  );
}

/** Thumbnail for an asset row. */
function AssetRowThumb({ asset, fit }: { asset: BrowseAssetDTO; fit: boolean }) {
  const [errored, setErrored] = useState(false);
  const isVideo = asset.mediaType === "video";
  if (errored) {
    return (
      <div className="flex h-9 w-9 flex-none items-center justify-center rounded bg-zinc-900">
        {isVideo ? <FilmIcon className="h-4 w-4 text-zinc-700" /> : <PhotoIcon className="h-4 w-4 text-zinc-700" />}
      </div>
    );
  }
  return (
    <img
      src={`/thumb/${asset.id}`}
      loading="lazy"
      alt={asset.filename}
      onError={() => setErrored(true)}
      className={`h-9 w-9 flex-none rounded bg-zinc-900 ${fit ? "object-contain" : "object-cover"}`}
    />
  );
}

/**
 * RenameFolderDialog — a small modal that edits a date-event folder's label.
 * The date prefix is fixed; only the label after it changes. An empty label
 * yields a bare YYYY-MM-DD folder.
 */
function RenameFolderDialog({
  relDir,
  initialLabel,
  onCancel,
  onSubmit,
}: {
  relDir: string;
  initialLabel: string;
  onCancel: () => void;
  onSubmit: (label: string) => void;
}) {
  const [label, setLabel] = useState(initialLabel);
  const datePart = relDir.slice(relDir.lastIndexOf("/") + 1, relDir.lastIndexOf("/") + 11);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onCancel();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onCancel]);

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4">
      <div className="absolute inset-0 bg-black/50" onClick={onCancel} />
      <div className="relative w-full max-w-md rounded-xl border border-zinc-700 bg-zinc-900 p-5 shadow-2xl">
        <h3 className="text-[15px] font-semibold text-zinc-100">Rename event folder</h3>
        <p className="mt-1 text-[12px] text-zinc-500">
          The date stays <span className="font-mono text-zinc-400">{datePart}</span>; only the label changes. This is
          the sanctioned way to rename — never rename archive folders in Finder.
        </p>
        <div className="mt-4">
          <label className="mb-1 block text-[11px] font-medium text-zinc-500">Label</label>
          <input
            autoFocus
            value={label}
            onChange={(e) => setLabel(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") onSubmit(label);
            }}
            placeholder="Leave empty for a bare date folder"
            className="w-full rounded-md border border-zinc-700 bg-zinc-950 px-3 py-2 text-[13px] text-zinc-200 outline-none focus:border-blue-500"
          />
          <p className="mt-1.5 font-mono text-[11px] text-zinc-600">
            → {label.trim() ? `${datePart} ${label.trim()}` : datePart}
          </p>
        </div>
        <div className="mt-5 flex justify-end gap-2">
          <Button size="sm" variant="secondary" onClick={onCancel}>
            Cancel
          </Button>
          <Button size="sm" variant="primary" icon={CheckIcon} onClick={() => onSubmit(label)}>
            Rename
          </Button>
        </div>
      </div>
    </div>
  );
}

interface Crumb {
  label: string;
  path: string;
}
/** Builds breadcrumb segments from a root-relative folder path. */
function breadcrumbs(relDir: string): Crumb[] {
  const crumbs: Crumb[] = [{ label: "Library", path: "" }];
  if (!relDir) return crumbs;
  const parts = relDir.split("/");
  let acc = "";
  for (const p of parts) {
    acc = acc ? `${acc}/${p}` : p;
    crumbs.push({ label: p, path: acc });
  }
  return crumbs;
}

/** The label portion of a "YYYY-MM-DD Label" folder name (empty for a bare date). */
function labelOfDateFolder(name: string): string {
  return name.length > 10 ? name.slice(10).trim() : "";
}
