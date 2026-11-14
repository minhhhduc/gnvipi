import type { TokenList, TokenRecord } from "./types";

/** The console never asks for more than the API will serve in one page. */
export const DEFAULT_TOKEN_LIMIT = 50;
export const MAX_TOKEN_LIMIT = 200;

/** A page size from the UI or the URL, normalized to the API's range. */
export function clampLimit(value: number): number {
  if (!Number.isFinite(value)) return DEFAULT_TOKEN_LIMIT;
  const whole = Math.floor(value);
  if (whole < 1) return DEFAULT_TOKEN_LIMIT;
  return Math.min(whole, MAX_TOKEN_LIMIT);
}

export interface PageWindow {
  page: number;
  from: number;
  to: number;
  hasPrev: boolean;
  hasNext: boolean;
}

/**
 * The visible page window for a limit/offset list. The users endpoint reports a
 * total; the tokens endpoint does not, so a full page is the only signal that
 * another page may exist.
 */
export function pageWindow(input: {
  limit: number;
  offset: number;
  count: number;
  total?: number | null;
}): PageWindow {
  const { limit, offset, count } = input;
  const total = input.total ?? null;
  const empty = count === 0;
  return {
    page: Math.floor(offset / limit) + 1,
    from: empty ? 0 : offset + 1,
    to: empty ? 0 : offset + count,
    hasPrev: offset > 0,
    hasNext: total == null ? count === limit : offset + count < total,
  };
}

export type TokenStatus = "active" | "expired" | "revoked";

export function tokenStatus(
  token: { revoked_at?: string | null; expires_at?: string | null },
  now: number = Date.now(),
): TokenStatus {
  if (token.revoked_at) return "revoked";
  const expiresAt = token.expires_at ? Date.parse(token.expires_at) : NaN;
  if (Number.isFinite(expiresAt) && expiresAt <= now) return "expired";
  return "active";
}

function calendarDate(value: string): string {
  const t = Date.parse(value);
  if (!Number.isFinite(t)) return "—";
  return new Date(t).toLocaleDateString([], { year: "numeric", month: "short", day: "numeric" });
}

/** A gateway token without an expiry is shown as such, not as an empty cell. */
export function expiryLabel(expiresAt?: string | null): string {
  if (!expiresAt) return "Never";
  return calendarDate(expiresAt);
}

export function dateLabel(value?: string | null): string {
  if (!value) return "—";
  return calendarDate(value);
}

export function lastUsedLabel(value?: string | null): string {
  if (!value) return "Never used";
  return calendarDate(value);
}

export type ExpiryChoice = "default" | "none";

/** Admin tokens always expire; only gateway tokens may opt out. */
export function expiryChoiceAllowed(kind: string, choice: ExpiryChoice): boolean {
  return choice === "default" || kind === "gateway";
}

export function tokenCreatePayload(input: {
  name: string;
  kind: string;
  scope?: string;
  userId?: string;
  expiry: ExpiryChoice;
}): Record<string, unknown> {
  const name = input.name.trim();
  if (!name) throw new Error("Token name is required");
  if (!expiryChoiceAllowed(input.kind, input.expiry)) {
    throw new Error("Admin tokens require an expiry");
  }
  const body: Record<string, unknown> = { name, kind: input.kind };
  if (input.kind === "admin") body.scope = input.scope;
  // Omitting both fields leaves the 365-day gateway default in place.
  if (input.expiry === "none") body.no_expiry = true;
  if (input.userId) body.user_id = input.userId;
  return body;
}

function optionalString(value: unknown): string | null {
  return typeof value === "string" && value ? value : null;
}

function requiredString(value: unknown, field: string): string {
  if (typeof value !== "string" || !value) throw new Error(`Invalid tokens response: ${field}`);
  return value;
}

/** Only the display fields cross into the UI; the plaintext and hash never do. */
function tokenRecord(value: unknown): TokenRecord {
  if (!value || typeof value !== "object") throw new Error("Invalid tokens response: token");
  const raw = value as Record<string, unknown>;
  return {
    id: requiredString(raw.id, "id"),
    user_id: requiredString(raw.user_id, "user_id"),
    kind: requiredString(raw.kind, "kind"),
    name: requiredString(raw.name, "name"),
    token_prefix: requiredString(raw.token_prefix, "token_prefix"),
    scope: optionalString(raw.scope),
    expires_at: optionalString(raw.expires_at),
    last_used_at: optionalString(raw.last_used_at),
    revoked_at: optionalString(raw.revoked_at),
    created_at: optionalString(raw.created_at),
  };
}

export function parseTokenList(value: unknown): TokenList {
  if (!value || typeof value !== "object") throw new Error("Invalid tokens response");
  const body = value as Record<string, unknown>;
  if (!Array.isArray(body.tokens)) throw new Error("Invalid tokens response");
  return {
    tokens: body.tokens.map(tokenRecord),
    limit: typeof body.limit === "number" ? body.limit : DEFAULT_TOKEN_LIMIT,
    offset: typeof body.offset === "number" ? body.offset : 0,
  };
}

/** The create response is the one place the plaintext token is ever visible. */
export function parseCreatedToken(value: unknown): { token: string; record: TokenRecord } {
  if (!value || typeof value !== "object") throw new Error("Invalid created token response");
  const raw = value as Record<string, unknown>;
  if (typeof raw.token !== "string" || !raw.token) throw new Error("Invalid created token response");
  return { token: raw.token, record: tokenRecord(raw) };
}

