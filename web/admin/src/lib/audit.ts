import { optionalString } from "./admin";

/** One audit row. Only the fields the screen renders cross this boundary. */
export interface AuditEntry {
  id: number;
  ts: string;
  actor_type: string;
  actor_id: string | null;
  user_id: string | null;
  key_id: string | null;
  provider_id: string | null;
  request_id: string | null;
  action: string;
  target_type: string;
  target_id: string | null;
  ip: string | null;
  details: Record<string, string | number | boolean>;
}

export interface AuditPage {
  entries: AuditEntry[];
  has_more: boolean;
  limit: number;
  next_cursor: string | null;
  before_cursor: string | null;
}

/** The cursor request and how deep the operator has paged. */
export interface AuditRequest {
  limit: number;
  after: string | null;
  before: string | null;
  depth: number;
}

/**
 * The server allowlist for audit details (internal/auth/audit.go). Unknown keys
 * and non-scalar values are dropped here too, so a widened response can never
 * put a secret into screen state or the DOM.
 */
const ALLOWED_DETAIL_KEYS = new Set([
  "id", "user_id", "key_id", "provider_id", "request_id",
  "target_id", "actor_id",
  "name", "username", "display_name", "provider_name",
  "key_name", "target_name", "model", "alias",
  "action", "status", "role", "kind", "scope",
  "enabled", "disabled", "success", "hidden", "deleted",
  "must_change", "active",
  "auth_scheme", "has_secret", "has_key_env", "has_key_ref",
  "count", "total", "revoked_sessions", "revoked_tokens",
]);

function auditScalar(value: unknown): value is string | number | boolean {
  if (typeof value === "string" || typeof value === "boolean") return true;
  return typeof value === "number" && Number.isFinite(value);
}

export function sanitizeDetails(value: unknown): Record<string, string | number | boolean> {
  if (!value || typeof value !== "object" || Array.isArray(value)) return {};
  const out: Record<string, string | number | boolean> = {};
  for (const [key, raw] of Object.entries(value as Record<string, unknown>)) {
    if (ALLOWED_DETAIL_KEYS.has(key.toLowerCase()) && auditScalar(raw)) out[key] = raw;
  }
  return out;
}

function requiredString(value: unknown, field: string): string {
  if (typeof value !== "string" || !value) throw new Error(`Invalid audit response: ${field}`);
  return value;
}

function auditEntry(value: unknown): AuditEntry {
  if (!value || typeof value !== "object") throw new Error("Invalid audit response: entry");
  const raw = value as Record<string, unknown>;
  if (typeof raw.id !== "number" || !Number.isFinite(raw.id)) {
    throw new Error("Invalid audit response: id");
  }
  return {
    id: raw.id,
    ts: requiredString(raw.ts, "ts"),
    actor_type: requiredString(raw.actor_type, "actor_type"),
    actor_id: optionalString(raw.actor_id),
    user_id: optionalString(raw.user_id),
    key_id: optionalString(raw.key_id),
    provider_id: optionalString(raw.provider_id),
    request_id: optionalString(raw.request_id),
    action: requiredString(raw.action, "action"),
    target_type: requiredString(raw.target_type, "target_type"),
    target_id: optionalString(raw.target_id),
    ip: optionalString(raw.ip),
    details: sanitizeDetails(raw.details),
  };
}

export function parseAuditPage(value: unknown): AuditPage {
  if (!value || typeof value !== "object") throw new Error("Invalid audit response");
  const body = value as Record<string, unknown>;
  if (!Array.isArray(body.entries)) throw new Error("Invalid audit response");
  return {
    entries: body.entries.map(auditEntry),
    has_more: body.has_more === true,
    limit: typeof body.limit === "number" && Number.isFinite(body.limit) ? body.limit : 50,
    next_cursor: optionalString(body.next_cursor),
    before_cursor: optionalString(body.before_cursor),
  };
}

/** Build the audit URL; the server rejects a request carrying both cursors. */
export function auditPath(input: { limit: number; after?: string | null; before?: string | null }): string {
  if (input.after && input.before) throw new Error("pass either after or before, not both");
  const params = new URLSearchParams({ limit: String(input.limit) });
  if (input.after) params.set("after", input.after);
  if (input.before) params.set("before", input.before);
  return `/admin/api/audit?${params.toString()}`;
}

export async function fetchAuditPage(
  request: { limit: number; after?: string | null; before?: string | null },
  fetcher: (path: string) => Promise<unknown>,
): Promise<AuditPage> {
  return parseAuditPage(await fetcher(auditPath(request)));
}

export function firstAuditRequest(limit: number): AuditRequest {
  return { limit, after: null, before: null, depth: 1 };
}

export function nextAuditRequest(request: AuditRequest, page: AuditPage): AuditRequest | null {
  // A backward response reports has_more in the newer direction, so it cannot
  // speak for the next page. Requesting backward means we came from a page
  // that still exists, so the forward link stays available.
  const forward = page.has_more || request.before != null;
  if (!forward || !page.next_cursor) return null;
  return { limit: request.limit, after: page.next_cursor, before: null, depth: request.depth + 1 };
}

export function previousAuditRequest(request: AuditRequest, page: AuditPage): AuditRequest | null {
  if (!page.before_cursor) return null;
  // A backward response's has_more is the newer direction: it says whether
  // another page exists above this one. A forward response came from such a
  // page, so it already proves one exists whatever its (older-direction)
  // has_more says.
  const backward = request.before != null;
  if (backward ? !page.has_more : request.after == null) return null;
  return { limit: request.limit, after: null, before: page.before_cursor, depth: Math.max(1, request.depth - 1) };
}

/** True row positions on the loaded cursor page; the client filter only hides rows. */
export function auditWindow(request: AuditRequest, page: AuditPage): { from: number; to: number } {
  const count = page.entries.length;
  if (count === 0) return { from: 0, to: 0 };
  const from = (request.depth - 1) * request.limit + 1;
  return { from, to: from + count - 1 };
}

/** Client-side filter over the loaded page; the API has no filter parameters. */
export function auditMatches(entry: AuditEntry, term: string): boolean {
  const needle = term.trim().toLowerCase();
  if (!needle) return true;
  const fields: (string | null)[] = [
    entry.action, entry.actor_type, entry.actor_id, entry.target_type,
    entry.target_id, entry.user_id, entry.key_id, entry.provider_id,
    entry.request_id, entry.ip,
  ];
  const haystack = [...fields, ...Object.values(entry.details)].map((value) => String(value ?? "").toLowerCase());
  return haystack.some((value) => value.includes(needle));
}

/** The rows the details cell renders; values are already allowlisted scalars. */
export function detailRows(entry: AuditEntry): { key: string; value: string }[] {
  return Object.entries(entry.details).map(([key, value]) => ({ key, value: String(value) }));
}

export function auditTime(value: string): string {
  const parsed = Date.parse(value);
  if (!Number.isFinite(parsed)) return "—";
  return new Date(parsed).toLocaleString([], {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}
