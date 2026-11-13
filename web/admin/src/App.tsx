import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import { api } from "./lib/api";
import { pageFromUrl, rawPageFromUrl } from "./lib/router";
import { GatewayContext, type Page } from "./lib/context";
import type { Stats } from "./lib/types";
import { Shell } from "./components/Shell";
import { ThemeContext, UiContext, type UiApi } from "./components/ui";
import { StatsView } from "./views/Stats";
import { ModelsView } from "./views/Models";
import { ConfigView } from "./views/Config";
import { ChromesView } from "./views/Chromes";

export function App() {
  const [page, setPage] = useState<Page>(() => pageFromUrl(location.search));
  const [theme, setTheme] = useState<"light" | "dark">(
    () => (document.documentElement.dataset.theme === "dark" ? "dark" : "light"),
  );
  const [stats, setStats] = useState<Stats | null>(null);
  const [statsUpdated, setStatsUpdated] = useState<number | null>(null);
  const [banner, setBannerState] = useState<{ message: string; retry?: () => void; owner?: string } | null>(null);
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

  const toast = useCallback((message: string, kind: "ok" | "bad" = "ok") => {
    const id = ++toastId.current;
    setToasts((prev) => [...prev, { id, message, kind }]);
    setTimeout(() => setToasts((prev) => prev.filter((t) => t.id !== id)), 3500);
  }, []);

  // pageFromUrl() falls back to Stats for anything it does not recognize, so
  // read the raw value too and say why the operator did not land where they
  // asked instead of silently rewriting their route.
  const syncFromUrl = useCallback(() => {
    const raw = rawPageFromUrl(location.search);
    const next = pageFromUrl(location.search);
    setPage(next);
    if (raw && raw !== next) toast(`Unknown page “${raw}” — showing Stats`, "bad");
  }, [toast]);

  useEffect(() => {
    syncFromUrl();
    window.addEventListener("popstate", syncFromUrl);
    return () => window.removeEventListener("popstate", syncFromUrl);
  }, [syncFromUrl]);

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
          owner: "stats",
        });
      });
  }, []);

  useEffect(() => {
    let timer: number | undefined;
    const syncPolling = () => {
      clearInterval(timer);
      timer = undefined;
      if (document.hidden) return;
      refreshStats();
      timer = setInterval(refreshStats, 2000);
    };

    syncPolling();
    document.addEventListener("visibilitychange", syncPolling);
    return () => {
      document.removeEventListener("visibilitychange", syncPolling);
      clearInterval(timer);
    };
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
    toast,
    setBanner: (message, retry, owner) =>
      setBannerState((current) => message ? { message, retry, owner } : !owner || current?.owner === owner ? null : current),
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
