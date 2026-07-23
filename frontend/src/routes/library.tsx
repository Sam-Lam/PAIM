import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ArrowPathIcon,
  ArrowsPointingInIcon,
  ArrowsPointingOutIcon,
  BoltIcon,
  CheckIcon,
  ChevronLeftIcon,
  ChevronRightIcon,
  ClipboardDocumentIcon,
  FilmIcon,
  FolderOpenIcon,
  MagnifyingGlassIcon,
  MagnifyingGlassMinusIcon,
  MagnifyingGlassPlusIcon,
  PhotoIcon,
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
  type MonthCountDTO,
  type ThumbsProgress,
  type WarmupStatusDTO,
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

/**
 * Library — a strictly read-only browse grid that proves what is archived and
 * surfaces provenance at a glance. No editing, ratings, albums, or export: PAIM
 * is an integrity tool, not a DAM.
 */
export function LibraryPage() {
  const toast = useToast();

  // Filters. The text query is debounced (300ms) into `query`; the rest apply
  // immediately.
  const [queryInput, setQueryInput] = useState("");
  const [query, setQuery] = useState("");
  const [mediaType, setMediaType] = useState("");
  const [verification, setVerification] = useState("");
  const [backup, setBackup] = useState("");
  const [month, setMonth] = useState("");
  // Tile rendering: crop to square (cover) or fit within it (contain). Persisted per machine.
  const [fitTiles, setFitTiles] = useState(() => localStorage.getItem("paim.library.fit") === "1");
  const toggleFit = () => {
    setFitTiles((v) => {
      localStorage.setItem("paim.library.fit", v ? "0" : "1");
      return !v;
    });
  };

  // Accumulated grid state ("Load more" pagination).
  const [items, setItems] = useState<BrowseAssetDTO[]>([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(0);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);

  const [selectedId, setSelectedId] = useState<string | null>(null);

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

  const filters = useMemo<BrowseFilters>(
    () => ({
      query,
      mediaType,
      verificationStatus: verification,
      backupStatus: backup,
      sessionId: "",
      yearMonth: month,
    }),
    [query, mediaType, verification, backup, month],
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
  }, [query, mediaType, verification, backup, month]);

  useEffect(() => {
    void months.run().catch(() => undefined);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const filtersActive = !!query || !!mediaType || !!verification || !!backup || !!month;
  const hasMore = items.length < total;
  const groups = useMemo(() => groupByMonth(items), [items]);

  const clearFilters = () => {
    setQueryInput("");
    setQuery("");
    setMediaType("");
    setVerification("");
    setBackup("");
    setMonth("");
  };

  return (
    <div>
      <PageHeader
        title="Library"
        description="A read-only view of everything archived — grouped by capture month, with full provenance on tap. Viewing only; nothing here modifies your files."
        actions={
          <div className="flex items-center gap-2">
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
              }}
              loading={loading && items.length > 0}
            >
              Refresh
            </Button>
          </div>
        }
      />

      <Card className="mb-5">
        <div className="flex flex-wrap items-end gap-3">
          <label className="min-w-[15rem] flex-1">
            <span className="mb-1 block text-[11px] font-medium text-zinc-500">Search</span>
            <div className="relative">
              <MagnifyingGlassIcon className="pointer-events-none absolute top-1/2 left-2.5 h-4 w-4 -translate-y-1/2 text-zinc-600" />
              <input
                value={queryInput}
                onChange={(e) => setQueryInput(e.target.value)}
                placeholder="Filename or path…"
                className="w-full rounded-md border border-zinc-700 bg-zinc-950 py-1.5 pr-3 pl-8 text-[13px] text-zinc-200 outline-none focus:border-blue-500"
              />
            </div>
          </label>

          <label>
            <span className="mb-1 block text-[11px] font-medium text-zinc-500">Type</span>
            <select value={mediaType} onChange={(e) => setMediaType(e.target.value)} className={SELECT_CLASS}>
              {MEDIA_TYPES.map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
            </select>
          </label>

          <label>
            <span className="mb-1 block text-[11px] font-medium text-zinc-500">Verification</span>
            <select value={verification} onChange={(e) => setVerification(e.target.value)} className={SELECT_CLASS}>
              {VERIFICATION.map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
            </select>
          </label>

          <label>
            <span className="mb-1 block text-[11px] font-medium text-zinc-500">Backup</span>
            <select value={backup} onChange={(e) => setBackup(e.target.value)} className={SELECT_CLASS}>
              {BACKUP.map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
            </select>
          </label>

          <label>
            <span className="mb-1 block text-[11px] font-medium text-zinc-500">Month</span>
            <select value={month} onChange={(e) => setMonth(e.target.value)} className={SELECT_CLASS}>
              <option value="">All months</option>
              {(months.data ?? []).map((m: MonthCountDTO) => (
                <option key={m.month} value={m.month}>
                  {formatMonthLong(m.month)} ({formatNumber(m.count)})
                </option>
              ))}
            </select>
          </label>

          {filtersActive ? (
            <Button size="sm" variant="ghost" icon={XMarkIcon} onClick={clearFilters}>
              Clear
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
                ? "No assets match these filters. Try clearing them."
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
                  <Thumb key={asset.id} asset={asset} fit={fitTiles} onClick={() => setSelectedId(asset.id)} />
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

      {selectedId ? (
        <DetailDrawer
          assetId={selectedId}
          items={items}
          onClose={() => setSelectedId(null)}
          onNavigate={setSelectedId}
        />
      ) : null}
    </div>
  );
}

/** A single grid tile: lazy 512 thumbnail with badges and a placeholder fallback. */
function Thumb({ asset, onClick, fit }: { asset: BrowseAssetDTO; onClick: () => void; fit: boolean }) {
  const [errored, setErrored] = useState(false);
  const isVideo = asset.mediaType === "video";
  return (
    <button
      onClick={onClick}
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
}: {
  assetId: string;
  items: BrowseAssetDTO[];
  onClose: () => void;
  onNavigate: (id: string) => void;
}) {
  const toast = useToast();
  const detail = useAsyncData<AssetDetailDTO>(() => BrowserService.AssetDetail(assetId));

  useEffect(() => {
    void detail.run().catch((e) => toast.fromError(e, "Failed to load asset"));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [assetId]);

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
