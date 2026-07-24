import { useEffect, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { ExclamationTriangleIcon } from "@heroicons/react/24/outline";
import { Button } from "./Button";

export interface ConfirmDialogProps {
  open: boolean;
  title: string;
  description?: ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  /** "danger" styles the confirm button red (default). */
  variant?: "danger" | "primary";
  /**
   * When set, the user must type this exact word to enable the confirm button.
   * Used for irreversible actions (e.g. type "DELETE").
   */
  requireWord?: string;
  /** Shows a spinner and disables buttons while the action runs. */
  loading?: boolean;
  /** Optional content rendered under the description while loading (e.g. a byte
   * progress bar for a long copy). */
  loadingContent?: ReactNode;
  /**
   * When set, renders an optional free-text reason field; its value is passed to
   * onConfirm. Used for Dismiss ("why are you dismissing this failure?"). Unlike
   * requireWord the field is optional — the confirm button stays enabled.
   */
  reasonLabel?: string;
  /** Placeholder for the optional reason field. */
  reasonPlaceholder?: string;
  /** Receives the optional reason text (empty string when none was entered). */
  onConfirm: (reason?: string) => void;
  onCancel: () => void;
}

export function ConfirmDialog({
  open,
  title,
  description,
  confirmLabel = "Confirm",
  cancelLabel = "Cancel",
  variant = "danger",
  requireWord,
  loading = false,
  loadingContent,
  reasonLabel,
  reasonPlaceholder,
  onConfirm,
  onCancel,
}: ConfirmDialogProps) {
  const [typed, setTyped] = useState("");
  const [reason, setReason] = useState("");

  // Reset the typed confirmation and reason whenever the dialog opens/closes.
  useEffect(() => {
    if (!open) {
      setTyped("");
      setReason("");
    }
  }, [open]);

  // Close on Escape.
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !loading) onCancel();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, loading, onCancel]);

  if (!open) return null;

  const wordOk = !requireWord || typed.trim().toUpperCase() === requireWord.toUpperCase();

  return createPortal(
    <div className="fixed inset-0 z-[90] flex items-center justify-center p-4">
      <div
        className="absolute inset-0 bg-black/60 backdrop-blur-sm"
        onClick={() => (loading ? null : onCancel())}
      />
      <div
        role="dialog"
        aria-modal="true"
        className="relative w-full max-w-md rounded-xl border border-zinc-800 bg-zinc-900 p-5 shadow-2xl shadow-black/50"
      >
        <div className="flex gap-3">
          {variant === "danger" ? (
            <div className="flex-none rounded-full bg-red-500/10 p-2">
              <ExclamationTriangleIcon className="h-5 w-5 text-red-400" />
            </div>
          ) : null}
          <div className="min-w-0 flex-1">
            <h2 className="text-sm font-semibold text-zinc-100">{title}</h2>
            {description != null ? (
              <div className="mt-1.5 text-[13px] leading-relaxed text-zinc-400">{description}</div>
            ) : null}

            {loading && loadingContent != null ? <div className="mt-3">{loadingContent}</div> : null}

            {requireWord ? (
              <div className="mt-3">
                <label className="mb-1 block text-xs text-zinc-500">
                  Type <span className="font-mono font-semibold text-zinc-300">{requireWord}</span> to confirm
                </label>
                <input
                  autoFocus
                  value={typed}
                  onChange={(e) => setTyped(e.target.value)}
                  className="w-full rounded-md border border-zinc-700 bg-zinc-950 px-2.5 py-1.5 text-[13px] text-zinc-100 outline-none focus:border-blue-500"
                  placeholder={requireWord}
                />
              </div>
            ) : null}

            {reasonLabel ? (
              <div className="mt-3">
                <label className="mb-1 block text-xs text-zinc-500">{reasonLabel}</label>
                <input
                  value={reason}
                  onChange={(e) => setReason(e.target.value)}
                  disabled={loading}
                  className="w-full rounded-md border border-zinc-700 bg-zinc-950 px-2.5 py-1.5 text-[13px] text-zinc-100 outline-none focus:border-blue-500"
                  placeholder={reasonPlaceholder ?? "Optional"}
                />
              </div>
            ) : null}
          </div>
        </div>

        <div className="mt-5 flex justify-end gap-2">
          <Button variant="secondary" onClick={onCancel} disabled={loading}>
            {cancelLabel}
          </Button>
          <Button
            variant={variant === "danger" ? "danger" : "primary"}
            onClick={() => onConfirm(reason.trim())}
            disabled={!wordOk}
            loading={loading}
          >
            {confirmLabel}
          </Button>
        </div>
      </div>
    </div>,
    document.body,
  );
}
