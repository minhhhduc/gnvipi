import type { Page } from "./context";
import type { AdminRole } from "./auth";

/** The ?page= value exactly as the URL carries it ("" when absent). */
export function rawPageFromUrl(search: string): string {
  return new URLSearchParams(search).get("page") ?? "";
}

const PAGE_ROLES: Record<Page, readonly AdminRole[]> = {
  // Dashboard owns the root path.
  stats: ["owner", "admin", "user"],
  // The per-request audit view stays with operators; users keep the dashboard.
  usage: ["owner", "admin", "user"],
  // Every role can test the models it can actually reach.
  playground: ["owner", "admin", "user"],
  // The API guide is reference material; every role holds its own key.
  api: ["owner", "admin", "user"],
  // Every role reaches Models, because that is where users manage their own
  // provider keys and per-key model aliases.
  models: ["owner", "admin", "user"],
  config: ["owner", "admin"],
  chromes: ["owner", "admin"],
  // Every role holds gateway tokens, so every role can reach its own list.
  tokens: ["owner", "admin", "user"],
  // Account administration is owner-only, including read access.
  users: ["owner"],
  // The audit trail is readable by owners only.
  audit: ["owner"],
};

const PAGE_PATHS: Record<Page, string> = {
  stats: "/",
  usage: "/usage",
  playground: "/playground",
  api: "/api",
  models: "/models",
  config: "/config",
  chromes: "/chromes",
  tokens: "/tokens",
  users: "/users",
  audit: "/audit",
};

const PATH_PAGES = Object.fromEntries(
  Object.entries(PAGE_PATHS).map(([page, path]) => [path, page as Page]),
) as Record<string, Page>;

export function pageAllowed(page: Page, role: AdminRole): boolean {
  return PAGE_ROLES[page].includes(role);
}

/** The clean URL a page lives at. */
export function pathForPage(page: Page): string {
  return PAGE_PATHS[page];
}

/** The page a clean pathname names; unknown paths fall back to the dashboard. */
export function pageFromPath(pathname: string, role?: AdminRole): Page {
  const path = pathname.length > 1 ? pathname.replace(/\/+$/, "") : pathname;
  const page = PATH_PAGES[path] ?? "stats";
  return role && !pageAllowed(page, role) ? "stats" : page;
}

/** Legacy ?page= links still resolve while their clean path is adopted. */
export function pageFromUrl(search: string, role?: AdminRole): Page {
  const value = rawPageFromUrl(search);
  const page = (value in PAGE_ROLES ? value : "stats") as Page;
  return role && !pageAllowed(page, role) ? "stats" : page;
}

/** Pathname first; a legacy ?page= wins so old links keep working. */
export function pageFromLocation(loc: { pathname: string; search: string }, role?: AdminRole): Page {
  return rawPageFromUrl(loc.search) ? pageFromUrl(loc.search, role) : pageFromPath(loc.pathname, role);
}