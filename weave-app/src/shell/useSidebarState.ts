import { useCallback, useEffect, useState } from "react";

const SIDEBAR_WIDTH_KEY = "weave.sidebarWidth";
const SIDEBAR_COLLAPSED_KEY = "weave.sidebarCollapsed";
const MIN_WIDTH = 200;
const MAX_WIDTH = 380;

function readNumber(key: string, fallback: number): number {
  try {
    const saved = window.localStorage.getItem(key);
    const parsed = saved ? Number.parseInt(saved, 10) : NaN;
    return Number.isFinite(parsed) ? parsed : fallback;
  } catch {
    return fallback;
  }
}

function readBool(key: string, fallback: boolean): boolean {
  try {
    const saved = window.localStorage.getItem(key);
    return saved === null ? fallback : saved === "1";
  } catch {
    return fallback;
  }
}

/**
 * Shared sidebar width + collapsed state across the app shell and the
 * control center. Persists to localStorage and stays in sync between
 * components via the `storage` event.
 */
export function useSidebarState() {
  const [width, setWidth] = useState<number>(() =>
    Math.max(MIN_WIDTH, Math.min(MAX_WIDTH, readNumber(SIDEBAR_WIDTH_KEY, 240))),
  );
  const [collapsed, setCollapsed] = useState<boolean>(() => readBool(SIDEBAR_COLLAPSED_KEY, false));

  useEffect(() => {
    window.localStorage.setItem(SIDEBAR_WIDTH_KEY, String(width));
  }, [width]);

  useEffect(() => {
    window.localStorage.setItem(SIDEBAR_COLLAPSED_KEY, collapsed ? "1" : "0");
  }, [collapsed]);

  useEffect(() => {
    function handleStorage(event: StorageEvent) {
      if (event.key === SIDEBAR_WIDTH_KEY) {
        const next = Math.max(MIN_WIDTH, Math.min(MAX_WIDTH, readNumber(SIDEBAR_WIDTH_KEY, 240)));
        setWidth(next);
      }
      if (event.key === SIDEBAR_COLLAPSED_KEY) {
        setCollapsed(readBool(SIDEBAR_COLLAPSED_KEY, false));
      }
    }
    window.addEventListener("storage", handleStorage);
    return () => window.removeEventListener("storage", handleStorage);
  }, []);

  const clampWidth = useCallback((next: number) => {
    setWidth(Math.max(MIN_WIDTH, Math.min(MAX_WIDTH, next)));
  }, []);

  return { width, setWidth: clampWidth, collapsed, setCollapsed, minWidth: MIN_WIDTH, maxWidth: MAX_WIDTH };
}
