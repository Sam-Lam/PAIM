import { useEffect, useRef, useState, useCallback } from "react";
import { Events } from "@wailsio/runtime";

/**
 * Subscribe to a Wails runtime event for the lifetime of the component.
 * The handler receives the event payload (`ev.data`) directly. The handler is
 * kept in a ref so callers may pass an inline closure without re-subscribing.
 */
export function useWailsEvent<T = unknown>(
  name: string,
  handler: (data: T) => void,
  enabled = true,
): void {
  const ref = useRef(handler);
  ref.current = handler;

  useEffect(() => {
    if (!enabled) return;
    // Events.On returns an unsubscribe function.
    const off = Events.On(name, (ev: { data: T }) => {
      ref.current(ev.data);
    });
    return () => {
      off();
    };
  }, [name, enabled]);
}

/**
 * Call `fn` on mount and then every `ms` milliseconds. `fn` is kept in a ref so
 * a fresh closure runs each tick without resetting the interval. Pass ms <= 0
 * to disable polling (still runs once on mount).
 */
export function usePoll(fn: () => void, ms: number, enabled = true): void {
  const ref = useRef(fn);
  ref.current = fn;

  useEffect(() => {
    if (!enabled) return;
    ref.current();
    if (ms <= 0) return;
    const id = setInterval(() => ref.current(), ms);
    return () => clearInterval(id);
  }, [ms, enabled]);
}

export interface AsyncState<T> {
  data: T | null;
  loading: boolean;
  error: string | null;
}

/**
 * Minimal async data helper: tracks loading/error/data and exposes a `run`
 * that callers can invoke on mount, on events, or on a poll. It does not call
 * automatically — the caller decides when (keeps event/poll wiring explicit).
 */
export function useAsyncData<T>(loader: () => Promise<T>): AsyncState<T> & {
  run: (opts?: { silent?: boolean }) => Promise<void>;
  setData: (data: T | null) => void;
} {
  const [state, setState] = useState<AsyncState<T>>({ data: null, loading: true, error: null });
  const loaderRef = useRef(loader);
  loaderRef.current = loader;

  const run = useCallback(async (opts?: { silent?: boolean }) => {
    if (!opts?.silent) setState((s) => ({ ...s, loading: true, error: null }));
    try {
      const data = await loaderRef.current();
      setState({ data, loading: false, error: null });
    } catch (err) {
      setState((s) => ({ ...s, loading: false, error: errorMessage(err) }));
    }
  }, []);

  const setData = useCallback((data: T | null) => {
    setState((s) => ({ ...s, data }));
  }, []);

  return { ...state, run, setData };
}

/** Normalize any thrown value into a readable message. */
export function errorMessage(err: unknown): string {
  if (err == null) return "Unknown error";
  if (typeof err === "string") return err;
  if (err instanceof Error) return err.message;
  if (typeof err === "object" && "message" in err) {
    const m = (err as { message?: unknown }).message;
    if (typeof m === "string") return m;
  }
  try {
    return JSON.stringify(err);
  } catch {
    return String(err);
  }
}
