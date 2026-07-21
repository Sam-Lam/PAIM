import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import { LibraryService, type CurrentLibraryDTO } from "./api";

interface LibraryContextValue {
  /** The open library, or null when none is open (show Welcome). */
  current: CurrentLibraryDTO | null;
  /** True until the first Current() resolves. */
  loading: boolean;
  /** Re-fetch the current library (call after opening/creating/migrating). */
  refresh: () => Promise<void>;
}

const LibraryContext = createContext<LibraryContextValue>({
  current: null,
  loading: true,
  refresh: async () => {},
});

/** Provides the currently open library to the whole app and a refresh hook. */
export function LibraryProvider({ children }: { children: ReactNode }) {
  const [current, setCurrent] = useState<CurrentLibraryDTO | null>(null);
  const [loading, setLoading] = useState(true);

  const refresh = useCallback(async () => {
    try {
      const c = await LibraryService.Current();
      setCurrent(c ?? null);
    } catch {
      setCurrent(null);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  return <LibraryContext.Provider value={{ current, loading, refresh }}>{children}</LibraryContext.Provider>;
}

export function useLibrary(): LibraryContextValue {
  return useContext(LibraryContext);
}
