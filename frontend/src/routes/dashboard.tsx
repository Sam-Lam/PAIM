import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "@tanstack/react-router";
import {
  ArrowPathIcon,
  ArrowTrendingUpIcon,
  CircleStackIcon,
  CloudArrowUpIcon,
  DocumentDuplicateIcon,
  ExclamationTriangleIcon,
  FilmIcon,
  InboxArrowDownIcon,
  PhotoIcon,
  Squares2X2Icon,
} from "@heroicons/react/24/outline";
import {
  Button,
  Card,
  EmptyState,
  LoadingBlock,
  PageHeader,
  SafeToEraseBadge,
  Stat,
  StatusBadge,
} from "../components";
import {
  DashboardService,
  WailsEvents,
  type AssetsOverTimeBucketDTO,
  type AssetsOverTimeDTO,
  type SourceDTO,
} from "../lib/api";
import { useAsyncData, usePoll, useWailsEvent } from "../lib/hooks";
import { useToast } from "../lib/toast";
import { formatBytes, formatNumber, formatRelative } from "../lib/format";

export function DashboardPage() {
  const toast = useToast();
  const { data: stats, loading, error, run } = useAsyncData(() => DashboardService.GetStats());

  // Poll every 30s (also fires the initial load), and refresh on relevant events.
  usePoll(() => {
    void run({ silent: true }).catch((e) => toast.fromError(e, "Failed to load dashboard"));
  }, 30_000);
  useWailsEvent(WailsEvents.ImportCompleted, () => void run({ silent: true }));
  useWailsEvent(WailsEvents.BackupQueueChanged, () => void run({ silent: true }));

  const refresh = () => void run().catch((e) => toast.fromError(e, "Failed to load dashboard"));

  return (
    <div>
      <PageHeader
        title="Dashboard"
        description="Library health at a glance — assets, storage, pending work, and recent activity."
        actions={
          <Button icon={ArrowPathIcon} onClick={refresh} loading={loading && !!stats}>
            Refresh
          </Button>
        }
      />

      {loading && !stats ? (
        <LoadingBlock label="Loading dashboard…" />
      ) : error && !stats ? (
        <EmptyState
          icon={ExclamationTriangleIcon}
          title="Could not load the dashboard"
          description={error}
          action={<Button onClick={refresh}>Try again</Button>}
        />
      ) : stats ? (
        <div className="space-y-6">
          {/* Stat grid */}
          <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
            <Stat label="Total Assets" value={formatNumber(stats.totals.assets)} icon={Squares2X2Icon} />
            <Stat label="Photos" value={formatNumber(stats.totals.photos)} icon={PhotoIcon} />
            <Stat label="Videos" value={formatNumber(stats.totals.videos)} icon={FilmIcon} />
            <Stat
              label="Storage Used"
              value={formatBytes(stats.totals.storageBytes)}
              icon={CircleStackIcon}
            />
            <Stat
              label="Pending Imports"
              value={formatNumber(stats.pendingImports)}
              icon={InboxArrowDownIcon}
              tone={stats.pendingImports > 0 ? "warn" : "default"}
            />
            <Stat
              label="Pending Backups"
              value={formatNumber(stats.backupQueue.pending)}
              icon={CloudArrowUpIcon}
              tone={stats.backupQueue.pending > 0 ? "accent" : "default"}
              hint={
                stats.backupQueue.mirrorPending > 0
                  ? `mirror uploads pending: ${formatNumber(stats.backupQueue.mirrorPending)}`
                  : undefined
              }
            />
            <Stat
              label="Failed Backups"
              value={formatNumber(stats.backupQueue.failed)}
              icon={ExclamationTriangleIcon}
              tone={stats.backupQueue.failed > 0 ? "danger" : "default"}
            />
            <Stat
              label="Duplicates"
              value={formatNumber(stats.duplicateCount)}
              icon={DocumentDuplicateIcon}
              tone={stats.duplicateCount > 0 ? "warn" : "default"}
            />
          </div>

          {/* Assets over time (by capture date) */}
          <AssetsOverTimeCard />

          {/* Sources row */}
          <div className="grid gap-3 lg:grid-cols-2">
            <Card
              title="Recently Connected Sources"
              actions={
                <Link to="/sources" className="text-xs font-medium text-blue-400 hover:text-blue-300">
                  View all
                </Link>
              }
            >
              <SourceList sources={stats.recentSources ?? []} empty="No sources connected yet." />
            </Card>

            <Card title="Safe To Erase" subtitle="Sources fully archived and backed up">
              <SourceList sources={stats.safeToEraseSources ?? []} empty="No sources are safe to erase yet." showSafe />
            </Card>
          </div>

          {/* Recent activity */}
          <Card
            title="Recent Activity"
            actions={
              <Link to="/logs" className="text-xs font-medium text-blue-400 hover:text-blue-300">
                Open logs
              </Link>
            }
            flush
          >
            {(stats.recentActivity ?? []).length === 0 ? (
              <EmptyState title="No recent activity" description="Import or backup activity will show up here." />
            ) : (
              <ul className="divide-y divide-zinc-800/60">
                {(stats.recentActivity ?? []).map((entry) => (
                  <li key={entry.id} className="flex items-start gap-3 px-4 py-2.5">
                    <span className={`mt-1.5 h-2 w-2 flex-none rounded-full ${levelDot(entry.level)}`} />
                    <div className="min-w-0 flex-1">
                      <div className="flex items-center gap-2">
                        <span className="rounded bg-zinc-800 px-1.5 py-0.5 text-[10px] font-medium tracking-wide text-zinc-400 uppercase">
                          {entry.subsystem || "system"}
                        </span>
                        <span className="text-[11px] text-zinc-500">{formatRelative(entry.timestamp)}</span>
                      </div>
                      <p className="mt-0.5 truncate text-[13px] text-zinc-300" title={entry.message}>
                        {entry.message}
                      </p>
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </Card>
        </div>
      ) : null}
    </div>
  );
}

function levelDot(level: string): string {
  switch ((level || "").toLowerCase()) {
    case "error":
      return "bg-red-500";
    case "warn":
    case "warning":
      return "bg-amber-500";
    case "debug":
      return "bg-zinc-600";
    default:
      return "bg-blue-500";
  }
}

function SourceList({
  sources,
  empty,
  showSafe = false,
}: {
  sources: SourceDTO[];
  empty: string;
  showSafe?: boolean;
}) {
  if (!sources || sources.length === 0) {
    return <EmptyState title="Nothing here yet" description={empty} />;
  }
  return (
    <ul className="space-y-2">
      {sources.map((s) => (
        <li key={s.id} className="flex items-center justify-between gap-3 rounded-md border border-zinc-800 px-3 py-2">
          <div className="min-w-0">
            <div className="truncate text-[13px] font-medium text-zinc-200">
              {s.volumeLabel || s.model || s.hardwareSerial || "Unknown source"}
            </div>
            <div className="mt-0.5 flex items-center gap-2 text-[11px] text-zinc-500">
              <StatusBadge status={s.sourceType} tone="muted" />
              <span>Last seen {formatRelative(s.lastSeenAt)}</span>
            </div>
            {showSafe && s.safeToEraseReason ? (
              <p className="mt-1 truncate text-[11px] text-emerald-400/80" title={s.safeToEraseReason}>
                {s.safeToEraseReason}
              </p>
            ) : null}
          </div>
          {showSafe ? (
            <SafeToEraseBadge safe={s.safeToErase} />
          ) : (
            <span className="flex-none text-[11px] text-zinc-500">{formatNumber(s.importCount)} imports</span>
          )}
        </li>
      ))}
    </ul>
  );
}

/* -------------------------------------------------------------------------- */
/* Assets over time                                                           */
/* -------------------------------------------------------------------------- */

// Series colors. Photos = blue-500, videos = violet-400 (app palette). Identity is
// carried by more than color: the stacked segments keep a fixed order (photos below
// videos) separated by a surface gap, the legend labels both series, and every bar
// has a hover tooltip — so the two are distinguishable without relying on hue.
const COLOR_PHOTOS = "#3b82f6"; // blue-500
const COLOR_VIDEOS = "#a78bfa"; // violet-400

const GRAN_OPTIONS: { value: Granularity; label: string }[] = [
  { value: "day", label: "Day" },
  { value: "month", label: "Month" },
  { value: "year", label: "Year" },
  { value: "5year", label: "5 Years" },
  { value: "all", label: "All" },
];
type Granularity = "day" | "month" | "year" | "5year" | "all";
const GRAN_KEY = "paim.dashboard.timeGranularity";

function loadGranularity(): Granularity {
  const raw = localStorage.getItem(GRAN_KEY);
  if (raw && GRAN_OPTIONS.some((o) => o.value === raw)) return raw as Granularity;
  return "all";
}

/**
 * AssetsOverTimeCard — a stacked photos/videos bar chart of the library's
 * distribution over CAPTURE date (COALESCE(capture_date, import_date)), bucketed at
 * a chosen granularity. This replaces the old import-date "growth" chart, which
 * collapsed a decades-old library imported in one month into a single bar.
 *
 * It owns its own granularity + data fetch (persisted per machine) so the parent's
 * GetStats polling is untouched, and refetches when an import completes.
 */
function AssetsOverTimeCard() {
  const toast = useToast();
  const navigate = useNavigate();
  const [gran, setGran] = useState<Granularity>(() => loadGranularity());
  const { data, error, run } = useAsyncData<AssetsOverTimeDTO | null>(() =>
    DashboardService.AssetsOverTime(gran),
  );

  useEffect(() => {
    void run({ silent: true }).catch((e) => toast.fromError(e, "Failed to load assets over time"));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [gran]);
  useWailsEvent(WailsEvents.ImportCompleted, () => void run({ silent: true }));

  const chooseGran = (g: Granularity) => {
    localStorage.setItem(GRAN_KEY, g);
    setGran(g);
  };

  const buckets = data?.buckets ?? [];
  const resolved = data?.granularity ?? gran;
  const monthMode = resolved === "month";
  const totals = useMemo(
    () =>
      buckets.reduce(
        (a, b) => ({ photos: a.photos + (b.photos ?? 0), videos: a.videos + (b.videos ?? 0) }),
        { photos: 0, videos: 0 },
      ),
    [buckets],
  );

  const subtitle = data?.windowed
    ? resolved === "day"
      ? "By capture date · most recent 120 days"
      : "By capture date · most recent buckets"
    : "By capture date";

  const onBucketClick = useCallback(
    (b: AssetsOverTimeBucketDTO) => {
      if (!monthMode) return;
      const yearMonth = (b.start || "").slice(0, 7); // YYYY-MM
      if (/^\d{4}-\d{2}$/.test(yearMonth)) {
        void navigate({ to: "/library", search: { yearMonth } });
      }
    },
    [monthMode, navigate],
  );

  return (
    <Card
      title="Assets over time"
      subtitle={subtitle}
      actions={
        <div className="flex items-center rounded-md border border-zinc-700 bg-zinc-950 p-0.5">
          {GRAN_OPTIONS.map((o) => (
            <button
              key={o.value}
              onClick={() => chooseGran(o.value)}
              className={`rounded px-2 py-1 text-[12px] font-medium transition ${
                gran === o.value ? "bg-zinc-800 text-zinc-100" : "text-zinc-400 hover:text-zinc-200"
              }`}
            >
              {o.label}
            </button>
          ))}
        </div>
      }
    >
      {error && !data ? (
        <EmptyState
          icon={ExclamationTriangleIcon}
          title="Could not load the chart"
          description={error}
        />
      ) : buckets.length === 0 || totals.photos + totals.videos === 0 ? (
        <EmptyState
          icon={ArrowTrendingUpIcon}
          title="No assets to chart yet"
          description="Once you import media, its capture-date distribution will chart here."
        />
      ) : (
        <div>
          {/* Legend with per-series totals. */}
          <div className="mb-3 flex items-center gap-4 text-[12px] text-zinc-400">
            <LegendSwatch color={COLOR_PHOTOS} label="Photos" count={totals.photos} />
            <LegendSwatch color={COLOR_VIDEOS} label="Videos" count={totals.videos} />
          </div>

          <StackedBarChart
            buckets={buckets}
            granularity={resolved}
            monthMode={monthMode}
            onBucketClick={onBucketClick}
          />

          {(data?.totalUndatedFallback ?? 0) > 0 ? (
            <p className="mt-3 text-[11px] text-zinc-500">
              {formatNumber(data!.totalUndatedFallback)} asset
              {data!.totalUndatedFallback === 1 ? " has" : "s have"} no capture date — shown by
              import date.
            </p>
          ) : null}
        </div>
      )}
    </Card>
  );
}

function LegendSwatch({ color, label, count }: { color: string; label: string; count: number }) {
  return (
    <span className="flex items-center gap-1.5">
      <span className="h-2.5 w-2.5 rounded-[3px]" style={{ backgroundColor: color }} />
      <span className="text-zinc-300">{label}</span>
      <span className="text-zinc-500 tabular-nums">{formatNumber(count)}</span>
    </span>
  );
}

/** Tracks a div's pixel width via ResizeObserver (for a responsive SVG). */
function useElementWidth(): [React.RefObject<HTMLDivElement>, number] {
  const ref = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(0);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const update = () => setWidth(el.clientWidth);
    update();
    const ro = new ResizeObserver(update);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  return [ref, width];
}

const CHART_PLOT_H = 140; // bar area height (px)
const CHART_AXIS_H = 20; // x-axis label strip (px)
const CHART_PAD_LEFT = 34; // y-axis tick label gutter (px)
const CHART_PAD_TOP = 8;
const CHART_PAD_RIGHT = 4;

interface HoverState {
  index: number;
  cx: number; // bar center x, in px
}

/**
 * StackedBarChart — inline SVG, no chart library. Photos stack below videos in each
 * bucket; bars share the available width (min 2px, gaps scale) and the container
 * clips horizontal overflow so the axis always fits. y-axis shows ≤ 3 ticks; x-axis
 * labels are sparse (first/last plus year/month boundaries). Hover highlights a bar
 * and shows a small tooltip; in month mode bars are clickable (drill to the Library
 * month filter).
 */
function StackedBarChart({
  buckets,
  granularity,
  monthMode,
  onBucketClick,
}: {
  buckets: AssetsOverTimeBucketDTO[];
  granularity: string;
  monthMode: boolean;
  onBucketClick: (b: AssetsOverTimeBucketDTO) => void;
}) {
  const [ref, width] = useElementWidth();
  const [hover, setHover] = useState<HoverState | null>(null);

  const n = buckets.length;
  const plotW = Math.max(0, width - CHART_PAD_LEFT - CHART_PAD_RIGHT);
  const baseline = CHART_PAD_TOP + CHART_PLOT_H;
  const svgH = CHART_PLOT_H + CHART_AXIS_H + CHART_PAD_TOP;

  const max = useMemo(
    () => Math.max(1, ...buckets.map((b) => (b.photos ?? 0) + (b.videos ?? 0))),
    [buckets],
  );

  const slotW = n > 0 ? plotW / n : 0;
  const gap = Math.min(6, Math.max(0.5, slotW * 0.18));
  const barW = Math.max(2, Math.min(slotW, slotW - gap));

  const yTicks = useMemo(() => niceTicks(max), [max]);
  const xTicks = useMemo(() => axisTicks(buckets, granularity), [buckets, granularity]);
  const scaleY = (v: number) => (v / max) * CHART_PLOT_H;

  const hovered = hover ? buckets[hover.index] : null;

  return (
    <div ref={ref} className="relative w-full overflow-x-hidden">
      {width > 0 && n > 0 ? (
        <svg
          width={width}
          height={svgH}
          role="img"
          aria-label="Assets over time, stacked photos and videos per bucket"
          onMouseLeave={() => setHover(null)}
        >
          {/* y grid + tick labels */}
          {yTicks.map((v) => {
            const y = baseline - scaleY(v);
            return (
              <g key={`y${v}`}>
                <line
                  x1={CHART_PAD_LEFT}
                  x2={width - CHART_PAD_RIGHT}
                  y1={y}
                  y2={y}
                  stroke="#27272a"
                  strokeWidth={1}
                />
                <text
                  x={CHART_PAD_LEFT - 6}
                  y={y}
                  textAnchor="end"
                  dominantBaseline="middle"
                  className="tabular-nums"
                  fontSize={10}
                  fill="#71717a"
                >
                  {compactNumber(v)}
                </text>
              </g>
            );
          })}

          {/* baseline */}
          <line
            x1={CHART_PAD_LEFT}
            x2={width - CHART_PAD_RIGHT}
            y1={baseline}
            y2={baseline}
            stroke="#3f3f46"
            strokeWidth={1}
          />

          {/* bars */}
          {buckets.map((b, i) => {
            const slotX = CHART_PAD_LEFT + i * slotW;
            const x = slotX + (slotW - barW) / 2;
            const cx = slotX + slotW / 2;
            const photos = b.photos ?? 0;
            const videos = b.videos ?? 0;
            const ph = scaleY(photos);
            const vh = scaleY(videos);
            const segGap = ph > 0 && vh > 0 ? 2 : 0;
            const rx = Math.min(2, barW / 2);
            const photoY = baseline - ph;
            const videoY = photoY - segGap - vh;
            const isHover = hover?.index === i;
            return (
              <g key={i} opacity={hover && !isHover ? 0.55 : 1}>
                {photos > 0 ? (
                  <rect x={x} y={photoY} width={barW} height={ph} rx={videos > 0 ? 0 : rx} fill={COLOR_PHOTOS} />
                ) : null}
                {videos > 0 ? (
                  <rect x={x} y={videoY} width={barW} height={vh} rx={rx} fill={COLOR_VIDEOS} />
                ) : null}
                {/* full-slot transparent hit target (bigger than the mark) */}
                <rect
                  x={slotX}
                  y={CHART_PAD_TOP}
                  width={Math.max(slotW, 1)}
                  height={CHART_PLOT_H}
                  fill="transparent"
                  style={{ cursor: monthMode ? "pointer" : "default" }}
                  onMouseEnter={() => setHover({ index: i, cx })}
                  onClick={() => onBucketClick(b)}
                >
                  {monthMode ? <title>Open {b.label} in Library</title> : null}
                </rect>
              </g>
            );
          })}

          {/* x-axis labels (sparse) */}
          {xTicks.map((t) => {
            const cx = CHART_PAD_LEFT + t.index * slotW + slotW / 2;
            return (
              <text
                key={`x${t.index}`}
                x={cx}
                y={baseline + 14}
                textAnchor="middle"
                fontSize={10}
                fill="#71717a"
              >
                {t.text}
              </text>
            );
          })}
        </svg>
      ) : (
        <div style={{ height: svgH }} />
      )}

      {/* Tooltip */}
      {hovered && hover ? (
        <div
          className="pointer-events-none absolute z-10 -translate-x-1/2 rounded-md border border-zinc-700 bg-zinc-900/95 px-2.5 py-1.5 text-[11px] shadow-lg backdrop-blur"
          style={{
            left: Math.min(Math.max(hover.cx, 60), Math.max(60, width - 60)),
            top: 0,
          }}
        >
          <div className="mb-0.5 font-medium text-zinc-200">{hovered.label}</div>
          <div className="flex items-center gap-1.5 text-zinc-400">
            <span className="h-2 w-2 rounded-[2px]" style={{ backgroundColor: COLOR_PHOTOS }} />
            Photos <span className="text-zinc-200 tabular-nums">{formatNumber(hovered.photos ?? 0)}</span>
          </div>
          <div className="flex items-center gap-1.5 text-zinc-400">
            <span className="h-2 w-2 rounded-[2px]" style={{ backgroundColor: COLOR_VIDEOS }} />
            Videos <span className="text-zinc-200 tabular-nums">{formatNumber(hovered.videos ?? 0)}</span>
          </div>
        </div>
      ) : null}
    </div>
  );
}

/** Up to 3 y-axis tick values (0, mid, max) as nice-ish integers. */
function niceTicks(max: number): number[] {
  if (max <= 1) return [0, 1];
  const mid = Math.round(max / 2);
  const ticks = [0, mid, max];
  return ticks.filter((v, i) => ticks.indexOf(v) === i);
}

/** Compact integer for axis labels, e.g. 1.2k, 3.4M. */
function compactNumber(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}

const MONTH_SHORT = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

/**
 * axisTicks picks sparse x-axis label positions: the first and last bucket always,
 * plus interior boundaries appropriate to the granularity —
 *   - day:   month boundaries, labeled "Jul" (falls back to "Jul ’24" across years)
 *   - month: year boundaries, labeled "2019"
 *   - year / 5year: every bucket (its own label)
 * The result is thinned to ≤ ~12 labels so they never crowd.
 */
function axisTicks(
  buckets: AssetsOverTimeBucketDTO[],
  granularity: string,
): { index: number; text: string }[] {
  const n = buckets.length;
  if (n === 0) return [];
  const marks = new Map<number, string>();

  if (granularity === "day") {
    let prev = "";
    const multiYear = buckets[0].start.slice(0, 4) !== buckets[n - 1].start.slice(0, 4);
    for (let i = 0; i < n; i++) {
      const ym = buckets[i].start.slice(0, 7); // YYYY-MM
      if (ym !== prev) {
        const mo = Number(ym.slice(5, 7)) - 1;
        marks.set(i, multiYear ? `${MONTH_SHORT[mo]} ’${ym.slice(2, 4)}` : (MONTH_SHORT[mo] ?? ym));
        prev = ym;
      }
    }
  } else if (granularity === "month") {
    let prev = "";
    for (let i = 0; i < n; i++) {
      const y = buckets[i].start.slice(0, 4);
      if (y !== prev) {
        marks.set(i, y);
        prev = y;
      }
    }
  } else {
    for (let i = 0; i < n; i++) marks.set(i, buckets[i].label);
  }

  // Always label the first and last bucket with their own label.
  marks.set(0, buckets[0].label);
  marks.set(n - 1, buckets[n - 1].label);

  let entries = [...marks.entries()].sort((a, b) => a[0] - b[0]);
  const MAXL = 12;
  if (entries.length > MAXL) {
    const step = Math.ceil(entries.length / MAXL);
    entries = entries.filter((_, i) => i % step === 0 || i === entries.length - 1);
  }
  return entries.map(([index, text]) => ({ index, text }));
}
