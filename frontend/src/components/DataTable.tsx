import { Fragment, useState, type ReactNode } from "react";
import {
  flexRender,
  getCoreRowModel,
  getSortedRowModel,
  useReactTable,
  type ColumnDef,
  type SortingState,
} from "@tanstack/react-table";
import {
  ChevronDownIcon,
  ChevronRightIcon,
  ChevronUpDownIcon,
  ChevronUpIcon,
} from "@heroicons/react/24/outline";
import { EmptyState, type EmptyStateProps } from "./EmptyState";
import { Skeleton, LoadingBlock } from "./Spinner";

/** Column definition alias — a plain TanStack ColumnDef over TData. */
export type DataTableColumn<TData> = ColumnDef<TData, unknown>;

export interface DataTablePagination {
  /** 1-based current page. */
  page: number;
  pageSize: number;
  /** Total row count across all pages (server-side). */
  total: number;
  onPageChange: (page: number) => void;
}

export interface DataTableProps<TData> {
  data: TData[];
  columns: DataTableColumn<TData>[];
  /** Loading state — shows skeleton rows (or a block when there is no data yet). */
  loading?: boolean;
  /** Shown when there are zero rows and not loading. */
  emptyState?: EmptyStateProps;
  /** Enable client-side sorting (default true). Ignored columns can set enableSorting:false. */
  enableSorting?: boolean;
  initialSorting?: SortingState;
  /** Stable row id accessor. */
  getRowId?: (row: TData, index: number) => string;
  /** Render an expanded detail panel below a row. Presence enables the expander column. */
  renderSubRow?: (row: TData) => ReactNode;
  /** Row click handler (suppressed while a row is expandable — the row toggles instead). */
  onRowClick?: (row: TData) => void;
  /** Server-side pagination controls rendered under the table. */
  pagination?: DataTablePagination;
  dense?: boolean;
  className?: string;
}

export function DataTable<TData>({
  data,
  columns,
  loading = false,
  emptyState,
  enableSorting = true,
  initialSorting = [],
  getRowId,
  renderSubRow,
  onRowClick,
  pagination,
  dense = false,
  className = "",
}: DataTableProps<TData>) {
  const [sorting, setSorting] = useState<SortingState>(initialSorting);
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});

  const table = useReactTable({
    data,
    columns,
    state: { sorting },
    onSortingChange: setSorting,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: enableSorting ? getSortedRowModel() : undefined,
    enableSorting,
    getRowId,
    manualPagination: !!pagination,
  });

  const hasExpansion = !!renderSubRow;
  const colCount = table.getAllLeafColumns().length + (hasExpansion ? 1 : 0);
  const rows = table.getRowModel().rows;
  const cellPad = dense ? "px-3 py-1.5" : "px-3 py-2.5";

  if (loading && data.length === 0) {
    return (
      <div className={className}>
        <LoadingBlock />
      </div>
    );
  }

  return (
    <div className={className}>
      <div className="overflow-x-auto">
        <table className="w-full border-collapse text-left text-[13px]">
          <thead>
            <tr className="border-b border-zinc-800">
              {hasExpansion ? <th className="w-8" /> : null}
              {table.getHeaderGroups().map((hg) =>
                hg.headers.map((header) => {
                  const canSort = header.column.getCanSort();
                  const sorted = header.column.getIsSorted();
                  return (
                    <th
                      key={header.id}
                      className={`${cellPad} text-xs font-medium tracking-wide text-zinc-500 uppercase`}
                      style={{ width: header.getSize() !== 150 ? header.getSize() : undefined }}
                    >
                      {header.isPlaceholder ? null : canSort ? (
                        <button
                          onClick={header.column.getToggleSortingHandler()}
                          className="inline-flex items-center gap-1 transition hover:text-zinc-300"
                        >
                          {flexRender(header.column.columnDef.header, header.getContext())}
                          {sorted === "asc" ? (
                            <ChevronUpIcon className="h-3.5 w-3.5 text-blue-400" />
                          ) : sorted === "desc" ? (
                            <ChevronDownIcon className="h-3.5 w-3.5 text-blue-400" />
                          ) : (
                            <ChevronUpDownIcon className="h-3.5 w-3.5 text-zinc-600" />
                          )}
                        </button>
                      ) : (
                        flexRender(header.column.columnDef.header, header.getContext())
                      )}
                    </th>
                  );
                }),
              )}
            </tr>
          </thead>
          <tbody>
            {rows.length === 0 ? (
              <tr>
                <td colSpan={colCount}>
                  <EmptyState
                    title={emptyState?.title ?? "Nothing here yet"}
                    description={emptyState?.description}
                    icon={emptyState?.icon}
                    action={emptyState?.action}
                  />
                </td>
              </tr>
            ) : (
              rows.map((row) => {
                const isExpanded = !!expanded[row.id];
                const clickable = !!onRowClick && !hasExpansion;
                return (
                  <Fragment key={row.id}>
                    <tr
                      className={`border-b border-zinc-800/60 ${
                        clickable || hasExpansion ? "cursor-pointer hover:bg-zinc-800/40" : "hover:bg-zinc-800/20"
                      }`}
                      onClick={() => {
                        if (hasExpansion) {
                          setExpanded((e) => ({ ...e, [row.id]: !e[row.id] }));
                        } else if (onRowClick) {
                          onRowClick(row.original);
                        }
                      }}
                    >
                      {hasExpansion ? (
                        <td className={`${cellPad} text-zinc-500`}>
                          {isExpanded ? (
                            <ChevronDownIcon className="h-4 w-4" />
                          ) : (
                            <ChevronRightIcon className="h-4 w-4" />
                          )}
                        </td>
                      ) : null}
                      {row.getVisibleCells().map((cell) => (
                        <td key={cell.id} className={`${cellPad} align-middle text-zinc-300`}>
                          {flexRender(cell.column.columnDef.cell, cell.getContext())}
                        </td>
                      ))}
                    </tr>
                    {hasExpansion && isExpanded ? (
                      <tr className="border-b border-zinc-800/60 bg-zinc-950/40">
                        <td colSpan={colCount} className="px-4 py-3">
                          {renderSubRow!(row.original)}
                        </td>
                      </tr>
                    ) : null}
                  </Fragment>
                );
              })
            )}
          </tbody>
        </table>
      </div>

      {loading && data.length > 0 ? (
        <div className="space-y-2 px-3 py-2">
          <Skeleton className="h-4 w-full" />
        </div>
      ) : null}

      {pagination ? <PaginationControls {...pagination} /> : null}
    </div>
  );
}

function PaginationControls({ page, pageSize, total, onPageChange }: DataTablePagination) {
  const pageCount = Math.max(1, Math.ceil(total / Math.max(1, pageSize)));
  const from = total === 0 ? 0 : (page - 1) * pageSize + 1;
  const to = Math.min(total, page * pageSize);
  return (
    <div className="flex items-center justify-between border-t border-zinc-800 px-3 py-2.5 text-xs text-zinc-500">
      <span className="tabular-nums">
        {from}–{to} of {total.toLocaleString()}
      </span>
      <div className="flex items-center gap-2">
        <button
          disabled={page <= 1}
          onClick={() => onPageChange(page - 1)}
          className="rounded-md border border-zinc-700 px-2.5 py-1 text-zinc-300 transition enabled:hover:bg-zinc-800 disabled:opacity-40"
        >
          Previous
        </button>
        <span className="tabular-nums text-zinc-400">
          Page {page} / {pageCount}
        </span>
        <button
          disabled={page >= pageCount}
          onClick={() => onPageChange(page + 1)}
          className="rounded-md border border-zinc-700 px-2.5 py-1 text-zinc-300 transition enabled:hover:bg-zinc-800 disabled:opacity-40"
        >
          Next
        </button>
      </div>
    </div>
  );
}
