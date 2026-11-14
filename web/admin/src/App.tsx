import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import { api, ApiError } from "./lib/api";
import { pageAllowed, pageFromUrl, rawPageFromUrl } from "./lib/router";
import { authScreenFor, isOperatorRole, parseAuthSession, sessionFailureMode, type AdminUser, type AuthSession } from "./lib/auth";
import { createPoller, PollTimeoutError, type Poller } from "./lib/polling";
import { GatewayContext, type Page } from "./lib/context";
import type { Stats } from "./lib/types";
import { LoginScreen, PasswordChangeScreen } from "./components/Login";
import { Shell } from "./components/Shell";
import { Card, Empty, PageHeader, ThemeContext, UiContext, type UiApi } from "./components/ui";
import { StatsView } from "./views/Stats";
import { ModelsView } from "./views/Models";
import { ConfigView } from "./views/Config";
import { ChromesView } from "./views/Chromes";
import { TokensView } from "./views/Tokens";
import { UsersView } from "./views/Users";

/** Stats refresh cadence, and the point at which a request counts as stalled. */
const STATS_POLL_INTERVAL_MS = 2000;
const STATS_POLL_TIMEOUT_MS = 15000;

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
  const identityGeneration = useRef(0);
  const statsPoller = useRef<Poller | null>(null);
  const consoleReady = authChecked && user !== null && authScreenFor(user) === "console";
  const effectivePage = user && pageAllowed(page, user.role) ? page : "stats";
  const statsPollingAllowed = consoleReady && isOperatorRole(user?.role);

  const invalidateIdentityData = useCallback(() => {
    identityGeneration.current += 1;
    statsPoller.current?.stop();
    statsPoller.current = null;
    statsErrorShown.current = false;
    setStats(null);
    setStatsUpdated(null);
    setBannerState(null);
  }, []);

  const setIdentity = useCallback((nextUser: AdminUser, legacy: boolean) => {
    invalidateIdentityData();
    setLegacyAuth(legacy);
    setPage(pageFromUrl(location.search, nextUser.role));
    setUser(nextUser);
    setAuthError("");
  }, [invalidateIdentityData]);

  const loadSession = useCallback(() => {
    invalidateIdentityData();
    setUser(null);
    setAuthChecked(false);
    setAuthError("");
    setLegacyAuth(false);
    api<unknown>("/admin/api/auth/session", { redirectOn401: false })
      .then((value) => setIdentity(parseAuthSession(value).user, false))
      .catch((err) => {
        if (err instanceof ApiError) {
          const mode = sessionFailureMode(err.status);
          if (mode === "login") return;
          if (mode === "legacy") {
            setIdentity(LEGACY_ADMIN_USER, true);
            return;
          }
        }
        setAuthError(err instanceof Error ? err.message : "Could not check the admin session");
      })
      .finally(() => setAuthChecked(true));
  }, [invalidateIdentityData, setIdentity]);

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
          : effectivePage === "chromes" ? "Chrome" : effectivePage[0].toUpperCase() + effectivePage.slice(1);
    document.title = `${title} · Gateway Admin`;
  }, [authChecked, effectivePage, user]);

  const refreshStats = useCallback(() => {
    statsPoller.current?.refresh();
  }, []);

  useEffect(() => {
    if (!statsPollingAllowed) return;
    const poller = createPoller(async (signal) => {
      const generation = identityGeneration.current;
      try {
        const data = await api<Stats>("/admin/api/stats", { signal });
        if (signal.aborted || generation !== identityGeneration.current) return;
        setStats(data);
        setStatsUpdated(Date.now());
        statsErrorShown.current = false;
      } catch (err) {
        // Identity and visibility aborts are silent; a poll timeout is a failure.
        const timedOut = signal.reason instanceof PollTimeoutError;
        if (generation !== identityGeneration.current || (signal.aborted && !timedOut)) return;
        if (statsErrorShown.current) return;
        statsErrorShown.current = true;
        setBannerState({
          message: `Stats unavailable: ${err instanceof Error ? err.message : "unknown error"}`,
          retry: refreshStats,
          owner: "stats",
        });
      }
    }, { intervalMs: STATS_POLL_INTERVAL_MS, timeoutMs: STATS_POLL_TIMEOUT_MS });

    statsPoller.current = poller;
    const syncPolling = () => {
      if (document.hidden) poller.stop();
      else poller.start();
    };
    syncPolling();
    document.addEventListener("visibilitychange", syncPolling);
    return () => {
      document.removeEventListener("visibilitychange", syncPolling);
      poller.stop();
      if (statsPoller.current === poller) statsPoller.current = null;
    };
  }, [refreshStats, statsPollingAllowed]);

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
    invalidateIdentityData();
    setLegacyAuth(false);
    setUser(null);
    const url = new URL(location.href);
    url.pathname = "/admin";
    url.search = "";
    url.hash = "";
    history.replaceState(null, "", url);
  }, [invalidateIdentityData]);

  const authenticated = useCallback((session: AuthSession) => {
    setIdentity(session.user, false);
  }, [setIdentity]);

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

  const operator = isOperatorRole(user.role);
  // Every role holds gateway tokens; account administration is owner-only. The
  // router already rejects an unauthorised ?page=, so these branches are the
  // render half of the same gate.
  const body =
    effectivePage === "tokens" ? <TokensView user={user} /> :
    effectivePage === "users" && operator ? <UsersView user={user} /> :
    !operator ? (
      <>
        <PageHeader title="Stats" sub="Usage for this gateway account" />
        <Card>
          <Empty
            title="Per-user stats are not available yet"
            body="Request attribution is still pending. Operator statistics are never shown to user accounts."
          />
        </Card>
      </>
    ) :
    effectivePage === "models" ? <ModelsView /> :
    effectivePage === "config" ? <ConfigView /> :
    effectivePage === "chromes" ? <ChromesView /> :
    <StatsView />;

  return (
    <UiContext.Provider value={ui}>
      <GatewayContext.Provider value={{ stats, statsUpdated, refreshStats, navigate }}>
        <ThemeContext.Provider value={{ theme, toggle: toggleTheme }}>
          <Shell
            page={effectivePage}
            user={user}
            legacy={legacyAuth}
            onLogout={legacyAuth ? undefined : () => void logout().catch((err) => toast(`Sign out failed: ${err instanceof Error ? err.message : "unknown"}`, "bad"))}
          >
            {banner && (
              <div role="alert" class="mb-3 flex items-start gap-2.5 rounded-[10px] border border-bad bg-bad-soft px-[13px] py-2.5 text-xs">
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
            <div class="fixed bottom-5 right-5 z-50 flex flex-col gap-2" aria-live="polite" aria-relevant="additions">
              {toasts.map((item) => (
                <div role={item.kind === "bad" ? "alert" : "status"} key={item.id} class={`rounded-lg px-3.5 py-2.5 text-xs text-white shadow-lg ${item.kind === "bad" ? "bg-bad" : "bg-fg"}`}>
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
