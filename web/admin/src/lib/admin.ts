import { ApiError } from "./api";

/** Page sizes offered by the admin lists; 200 is also the server's page cap. */
export const PAGE_SIZES = [25, 50, 100, 200];
export const DEFAULT_LIMIT = 50;
export const MAX_LIMIT = 200;

/** A page size from the UI or the URL, normalized to the API's range. */
export function clampLimit(value: number): number {
  if (!Number.isFinite(value)) return DEFAULT_LIMIT;
  const whole = Math.floor(value);
  if (whole < 1) return DEFAULT_LIMIT;
  return Math.min(whole, MAX_LIMIT);
}

/** The last valid offset after a page's result set shrank (e.g. after a delete). */
export function clampOffset(input: { total: number; limit: number; offset: number }): number {
  if (input.offset <= 0) return 0;
  const pageSize = clampLimit(input.limit);
  const last = Math.max(0, Math.ceil(input.total / pageSize) - 1) * pageSize;
  return Math.min(input.offset, last);
}

export interface PageWindow {
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
    from: empty ? 0 : offset + 1,
    to: empty ? 0 : offset + count,
    hasPrev: offset > 0,
    hasNext: total == null ? count === limit : offset + count < total,
  };
}

/** One error string for every admin mutation: field errors name the field. */
export function message(err: unknown): string {
  if (err instanceof ApiError) {
    return err.body?.field ? `${err.body.field}: ${err.message}` : err.message;
  }
  return err instanceof Error ? err.message : "Request failed";
}

export function optionalString(value: unknown): string | null {
  return typeof value === "string" && value ? value : null;
}

export function dateLabel(value?: string | null): string {
  if (!value) return "—";
  const t = Date.parse(value);
  if (!Number.isFinite(t)) return "—";
  return new Date(t).toLocaleDateString([], { year: "numeric", month: "short", day: "numeric" });
}
