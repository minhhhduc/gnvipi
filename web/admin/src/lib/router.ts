import type { Page } from "./context";
import type { AdminRole } from "./auth";

/** The ?page= value exactly as the URL carries it ("" when absent). */
export function rawPageFromUrl(search: string): string {
  return new URLSearchParams(search).get("page") ?? "";
}

const PAGE_ROLES: Record<Page, readonly AdminRole[]> = {
  stats: ["owner", "admin", "user"],
  models: ["owner", "admin"],
  config: ["owner", "admin"],
  chromes: ["owner", "admin"],
  // Every role holds gateway tokens, so every role can reach its own list.
  tokens: ["owner", "admin", "user"],
  // Account administration is owner-only, including read access.
  users: ["owner"],
  // The audit trail is readable by owners only.
  audit: ["owner"],
};

export function pageAllowed(page: Page, role: AdminRole): boolean {
  return PAGE_ROLES[page].includes(role);
}

export function pageFromUrl(search: string, role?: AdminRole): Page {
  const value = rawPageFromUrl(search);
  const page: Page =
    value === "stats" ||
    value === "models" ||
    value === "config" ||
    value === "chromes" ||
    value === "tokens" ||
    value === "users" ||
    value === "audit"
      ? value
      : "stats";
  return role && !pageAllowed(page, role) ? "stats" : page;
}
