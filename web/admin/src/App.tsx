import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import { api } from "./lib/api";
import { GatewayContext, type Page } from "./lib/context";
import type { Stats } from "./lib/types";
import { Shell } from "./components/Shell";
import { ThemeContext, UiContext, type UiApi } from "./components/ui";
import { StatsView } from "./views/Stats";
import { ModelsView } from "./views/Models";
import { ConfigView } from "./views/Config";
import { ChromesView } from "./views/Chromes";

const PAGES: Page[] = ["stats", "models", "config", "chromes"];

function pageFromUrl(): Page {
  const value = new URLSearchParams(location.search).get("page") as Page | null;
  return value && PAGES.includes(value) ? value : "stats";
}

export function App() {
  const [page, setPage] = useState<Page>(pageFromUrl);
  const [theme, setTheme] = useState<"light" | "dark">(
    () => (document.documentElement.dataset.theme === "dark" ? "dark" : "light"),
  );
  const [stats, setStats] = useState<Stats | null>(null);
  const [statsUpdated, setStatsUpdated] = useState<number | null>(null);
  const [banner, setBannerState] = useState<{ message: string; retry?: () => void } | null>(null);
  const [toasts, setToasts] = useState<{ id: number; message: string; kind: "ok" | "bad" }[]>([]);
  const toastId = useRef(0);
  const statsErrorShown = useRef(false);

  const navigate = useCallback((next: Page) => {
    const url = new URL(location.href);
    url.searchParams.set("page", next);
    history.pushState({ page: next }, "", url);
    setPage(next);
    window.scrollTo(0, 0);
  }, []);

  useEffect(() => {
    const onPop = () => setPage(pageFromUrl());
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);

  useEffect(() => {
    document.title = `${page === "chromes" ? "Chrome" : page[0].toUpperCase() + page.slice(1)} · Gateway Admin`;
  }, [page]);

  const refreshStats = useCallback(() => {
    api<Stats>("/admin/api/stats")
      .then((data) => {
        setStats(data);
        setStatsUpdated(Date.now());
        statsErrorShown.current = false;
      })
      .catch((err) => {
        if (statsErrorShown.current) return;
        statsErrorShown.current = true;
        setBannerState({
          message: `Stats unavailable: ${err instanceof Error ? err.message : "unknown error"}`,
          retry: refreshStats,
        });
      });
  }, []);

  useEffect(() => {
    refreshStats();
    const timer = setInterval(refreshStats, 2000);
    return () => clearInterval(timer);
  }, [refreshStats]);

  const toggleTheme = () => {
    const next = theme === "dark" ? "light" : "dark";
    setTheme(next);
    document.documentElement.dataset.theme = next;
    try {
      localStorage.setItem("gw-theme", next);
    } catch {
      /* storage may be unavailable */
    }
  };

  const ui: UiApi = {
    toast: (message, kind = "ok") => {
      const id = ++toastId.current;
      setToasts((prev) => [...prev, { id, message, kind }]);
      setTimeout(() => setToasts((prev) => prev.filter((t) => t.id !== id)), 3500);
    },
    setBanner: (message, retry) => setBannerState(message ? { message, retry } : null),
  };

  const body =
    page === "models" ? <ModelsView /> :
    page === "config" ? <ConfigView /> :
    page === "chromes" ? <ChromesView /> :
    <StatsView />;

  return (
    <UiContext.Provider value={ui}>
      <GatewayContext.Provider value={{ stats, statsUpdated, refreshStats, navigate }}>
        <ThemeContext.Provider value={{ theme, toggle: toggleTheme }}>
        <Shell page={page}>
          {banner && (
            <div class="mb-3 flex items-start gap-2.5 rounded-[10px] border border-bad bg-bad-soft px-[13px] py-2.5 text-xs">
              <span class="text-bad">⚠</span>
              <div>
                <b class="text-bad">Request failed:</b> {banner.message}{" "}
                {banner.retry && (
                  <button class="font-semibold text-accent underline" onClick={banner.retry}>Retry</button>
                )}
              </div>
              <button class="ml-auto text-dim" aria-label="Dismiss error" onClick={() => setBannerState(null)}>×</button>
            </div>
          )}
          {body}
        </Shell>
        {toasts.length > 0 && (
          <div class="fixed bottom-5 right-5 z-50 flex flex-col gap-2">
            {toasts.map((t) => (
              <div key={t.id} class={`rounded-lg px-3.5 py-2.5 text-xs text-white shadow-lg ${t.kind === "bad" ? "bg-bad" : "bg-fg"}`}>
                {t.message}
              </div>
            ))}
          </div>
        )}
        </ThemeContext.Provider>
      </GatewayContext.Provider>
    </UiContext.Provider>
  );
}
