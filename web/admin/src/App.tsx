import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import { api, ApiError } from "./lib/api";
import { pageAllowed, pageFromUrl, rawPageFromUrl } from "./lib/router";
import { authScreenFor, sessionFailureMode, type AdminUser, type AuthSession } from "./lib/auth";
import { GatewayContext, type Page } from "./lib/context";
import type { Stats } from "./lib/types";
import { LoginScreen, PasswordChangeScreen } from "./components/Login";
import { Shell } from "./components/Shell";
import { ThemeContext, UiContext, type UiApi } from "./components/ui";
import { StatsView } from "./views/Stats";
import { ModelsView } from "./views/Models";
import { ConfigView } from "./views/Config";
import { ChromesView } from "./views/Chromes";

const LEGACY_ADMIN_USER: AdminUser = {
  id: "legacy-admin",
  username: "legacy-admin",
  email: "",
  display_name: "Legacy admin",
  role: "owner",
  status: "active",
  must_change_password: false,
};

export function App() {
  const [page, setPage] = useState<Page>(() => pageFromUrl(location.search));
  const [theme, setTheme] = useState<"light" | "dark">(
    () => (document.documentElement.dataset.theme === "dark" ? "dark" : "light"),
  );
  const [user, setUser] = useState<AdminUser | null>(null);
  const [authChecked, setAuthChecked] = useState(false);
  const [authError, setAuthError] = useState("");
  const [legacyAuth, setLegacyAuth] = useState(false);
  const [stats, setStats] = useState<Stats | null>(null);
  const [statsUpdated, setStatsUpdated] = useState<number | null>(null);
  const [banner, setBannerState] = useState<{ message: string; retry?: () => void; owner?: string } | null>(null);
  const [toasts, setToasts] = useState<{ id: number; message: string; kind: "ok" | "bad" }[]>([]);
  const toastId = useRef(0);
  const statsErrorShown = useRef(false);
  const consoleReady = user !== null && authScreenFor(user) === "console";

  const loadSession = useCallback(() => {
    setAuthChecked(false);
    setAuthError("");
    setLegacyAuth(false);
    api<AuthSession>("/admin/api/auth/session", { redirectOn401: false })
      .then((session) => setUser(session.user))
      .catch((err) => {
        if (err instanceof ApiError) {
          const mode = sessionFailureMode(err.status);
          if (mode === "login") {
            setUser(null);
            return;
          }
          if (mode === "legacy") {
            setLegacyAuth(true);
            setUser(LEGACY_ADMIN_USER);
            return;
          }
        }
        setAuthError(err instanceof Error ? err.message : "Could not check the admin session");
      })
      .finally(() => setAuthChecked(true));
  }, []);

  useEffect(loadSession, [loadSession]);

  const navigate = useCallback((requested: Page) => {
    const next = user && pageAllowed(requested, user.role) ? requested : "stats";
    const url = new URL(location.href);
    url.searchParams.set("page", next);
    history.pushState({ page: next }, "", url);
    setPage(next);
    window.scrollTo(0, 0);
  }, [user]);

  const toast = useCallback((message: string, kind: "ok" | "bad" = "ok") => {
    const id = ++toastId.current;
    setToasts((prev) => [...prev, { id, message, kind }]);
    setTimeout(() => setToasts((prev) => prev.filter((item) => item.id !== id)), 3500);
  }, []);

  const syncFromUrl = useCallback(() => {
    if (!user) return;
    const raw = rawPageFromUrl(location.search);
    const next = pageFromUrl(location.search, user.role);
    setPage(next);
    if (raw && raw !== next) toast(`Page “${raw}” is not available for ${user.role} accounts — showing Stats`, "bad");
  }, [toast, user]);

  useEffect(() => {
    syncFromUrl();
    window.addEventListener("popstate", syncFromUrl);
    return () => window.removeEventListener("popstate", syncFromUrl);
  }, [syncFromUrl]);

  useEffect(() => {
    const title = !authChecked
      ? "Loading"
      : !user
        ? "Sign in"
        : user.must_change_password
          ? "Change password"
          : page === "chromes" ? "Chrome" : page[0].toUpperCase() + page.slice(1);
    document.title = `${title} · Gateway Admin`;
  }, [authChecked, page, user]);

  const refreshStats = useCallback(() => {
    if (!consoleReady) return;
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
  }, [consoleReady]);

  useEffect(() => {
    if (!consoleReady) return;
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
  }, [consoleReady, refreshStats]);

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

  const logout = useCallback(async () => {
    await api<void>("/admin/api/auth/logout", { method: "POST" });
    setUser(null);
    setStats(null);
    setStatsUpdated(null);
    setBannerState(null);
    const url = new URL(location.href);
    url.pathname = "/admin";
    url.search = "";
    url.hash = "";
    history.replaceState(null, "", url);
  }, []);

  const authenticated = useCallback((session: AuthSession) => {
    setLegacyAuth(false);
    setUser(session.user);
    setPage(pageFromUrl(location.search, session.user.role));
    setAuthError("");
  }, []);

  const ui: UiApi = {
    toast,
    setBanner: (message, retry, owner) =>
      setBannerState((current) => message ? { message, retry, owner } : !owner || current?.owner === owner ? null : current),
  };

  if (!authChecked) {
    return <main class="grid min-h-screen place-items-center text-sm text-dim">Checking session…</main>;
  }
  if (authError) {
    return (
      <main class="grid min-h-screen place-items-center px-5">
        <div class="card w-full max-w-[400px] p-6 text-center">
          <h1 class="text-lg font-bold">Session unavailable</h1>
          <p class="mt-1 text-sm text-dim">{authError}</p>
          <button type="button" class="btn btn-primary mt-4" onClick={loadSession}>Try again</button>
        </div>
      </main>
    );
  }
  if (!user) {
    return <LoginScreen onAuthenticated={authenticated} />;
  }
  if (authScreenFor(user) === "password-change") {
    return <PasswordChangeScreen user={user} onChanged={setUser} onLogout={logout} />;
  }

  const body =
    page === "models" ? <ModelsView /> :
    page === "config" ? <ConfigView /> :
    page === "chromes" ? <ChromesView /> :
    <StatsView />;

  return (
    <UiContext.Provider value={ui}>
      <GatewayContext.Provider value={{ stats, statsUpdated, refreshStats, navigate }}>
        <ThemeContext.Provider value={{ theme, toggle: toggleTheme }}>
          <Shell
            page={page}
            user={user}
            onLogout={legacyAuth ? undefined : () => void logout().catch((err) => toast(`Sign out failed: ${err instanceof Error ? err.message : "unknown"}`, "bad"))}
          >
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
              {toasts.map((item) => (
                <div key={item.id} class={`rounded-lg px-3.5 py-2.5 text-xs text-white shadow-lg ${item.kind === "bad" ? "bg-bad" : "bg-fg"}`}>
                  {item.message}
                </div>
              ))}
            </div>
          )}
        </ThemeContext.Provider>
      </GatewayContext.Provider>
    </UiContext.Provider>
  );
}
