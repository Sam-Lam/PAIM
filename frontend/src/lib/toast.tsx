import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import {
  CheckCircleIcon,
  ExclamationTriangleIcon,
  InformationCircleIcon,
  XCircleIcon,
  XMarkIcon,
} from "@heroicons/react/24/outline";
import { errorMessage } from "./hooks";

export type ToastKind = "success" | "error" | "warn" | "info";

/** An optional inline action button shown on a toast (e.g. a deep-link). */
export interface ToastAction {
  label: string;
  onClick: () => void;
}

export interface Toast {
  id: number;
  kind: ToastKind;
  title: string;
  message?: string;
  action?: ToastAction;
}

interface ToastApi {
  push: (kind: ToastKind, title: string, message?: string, action?: ToastAction) => void;
  success: (title: string, message?: string, action?: ToastAction) => void;
  error: (title: string, message?: string, action?: ToastAction) => void;
  warn: (title: string, message?: string, action?: ToastAction) => void;
  info: (title: string, message?: string, action?: ToastAction) => void;
  /** Turn a caught value into an error toast. Returns the message for logging. */
  fromError: (err: unknown, title?: string) => string;
  dismiss: (id: number) => void;
}

const ToastContext = createContext<ToastApi | null>(null);

let seq = 1;

const KIND_META: Record<ToastKind, { icon: typeof CheckCircleIcon; ring: string; iconColor: string }> = {
  success: { icon: CheckCircleIcon, ring: "border-emerald-500/40", iconColor: "text-emerald-400" },
  error: { icon: XCircleIcon, ring: "border-red-500/40", iconColor: "text-red-400" },
  warn: { icon: ExclamationTriangleIcon, ring: "border-amber-500/40", iconColor: "text-amber-400" },
  info: { icon: InformationCircleIcon, ring: "border-blue-500/40", iconColor: "text-blue-400" },
};

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);

  const dismiss = useCallback((id: number) => {
    setToasts((t) => t.filter((x) => x.id !== id));
  }, []);

  const push = useCallback(
    (kind: ToastKind, title: string, message?: string, action?: ToastAction) => {
      const id = seq++;
      setToasts((t) => [...t, { id, kind, title, message, action }]);
      // An actionable toast lingers a little longer so the user can reach the button.
      const ttl = kind === "error" ? 8000 : action ? 9000 : 4500;
      setTimeout(() => dismiss(id), ttl);
    },
    [dismiss],
  );

  const api = useMemo<ToastApi>(
    () => ({
      push,
      success: (title, message, action) => push("success", title, message, action),
      error: (title, message, action) => push("error", title, message, action),
      warn: (title, message, action) => push("warn", title, message, action),
      info: (title, message, action) => push("info", title, message, action),
      fromError: (err, title = "Something went wrong") => {
        const msg = errorMessage(err);
        push("error", title, msg);
        return msg;
      },
      dismiss,
    }),
    [push, dismiss],
  );

  return (
    <ToastContext.Provider value={api}>
      {children}
      {createPortal(
        <div className="pointer-events-none fixed top-4 right-4 z-[100] flex w-80 max-w-[calc(100vw-2rem)] flex-col gap-2">
          {toasts.map((t) => {
            const meta = KIND_META[t.kind];
            const Icon = meta.icon;
            return (
              <div
                key={t.id}
                className={`pointer-events-auto flex items-start gap-3 rounded-lg border ${meta.ring} bg-zinc-900/95 p-3 shadow-lg shadow-black/40 backdrop-blur`}
              >
                <Icon className={`mt-0.5 h-5 w-5 flex-none ${meta.iconColor}`} />
                <div className="min-w-0 flex-1">
                  <p className="text-[13px] font-medium text-zinc-100">{t.title}</p>
                  {t.message ? (
                    <p className="selectable mt-0.5 break-words text-xs text-zinc-400">{t.message}</p>
                  ) : null}
                  {t.action ? (
                    <button
                      onClick={() => {
                        t.action?.onClick();
                        dismiss(t.id);
                      }}
                      className={`mt-1.5 text-xs font-semibold ${meta.iconColor} hover:underline`}
                    >
                      {t.action.label}
                    </button>
                  ) : null}
                </div>
                <button
                  onClick={() => dismiss(t.id)}
                  className="flex-none rounded p-0.5 text-zinc-500 transition hover:text-zinc-200"
                  aria-label="Dismiss"
                >
                  <XMarkIcon className="h-4 w-4" />
                </button>
              </div>
            );
          })}
        </div>,
        document.body,
      )}
    </ToastContext.Provider>
  );
}

export function useToast(): ToastApi {
  const ctx = useContext(ToastContext);
  if (!ctx) throw new Error("useToast must be used within <ToastProvider>");
  return ctx;
}
