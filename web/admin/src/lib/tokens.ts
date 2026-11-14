import type { TokenList, TokenRecord } from "./types";
import { DEFAULT_LIMIT, clampOffset, dateLabel, optionalString, requiredString } from "./admin";

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

/** A gateway token without an expiry is shown as such, not as an empty cell. */
export function expiryLabel(expiresAt?: string | null): string {
  return expiresAt ? dateLabel(expiresAt) : "Never";
}

export function lastUsedLabel(value?: string | null): string {
  return value ? dateLabel(value) : "Never used";
}

export type ExpiryChoice = "default" | "none";

/** Admin tokens always expire; only gateway tokens may opt out. */
export function expiryChoiceAllowed(kind: string, choice: ExpiryChoice): boolean {
  return choice === "default" || kind === "gateway";
}

/** The label for the "default" option; the server picks a different one per kind. */
export function defaultExpiryLabel(kind: string): string {
  return kind === "admin" ? "90 days (default)" : "365 days (default)";
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
  // The server counts runes and caps the name at 100.
  if ([...name].length > 100) throw new Error("Token name must be at most 100 characters");
  if (!expiryChoiceAllowed(input.kind, input.expiry)) {
    throw new Error("Admin tokens require an expiry");
  }
  const body: Record<string, unknown> = { name, kind: input.kind };
  if (input.kind === "admin") body.scope = input.scope;
  // Omitting both fields leaves the server default in place (365 days gateway,
  // 90 days admin).
  if (input.expiry === "none") body.no_expiry = true;
  if (input.userId) body.user_id = input.userId;
  return body;
}

/** The token endpoint has no total, so an empty page past the first means the
 *  result set shrank; step back one page. */
export function emptyPageOffset(limit: number, offset: number): number {
  return offset > 0 ? clampOffset({ total: offset, limit, offset }) : 0;
}

/** Only the display fields cross into the UI; the plaintext and hash never do. */
function tokenRecord(value: unknown): TokenRecord {
  if (!value || typeof value !== "object") throw new Error("Invalid tokens response: token");
  const raw = value as Record<string, unknown>;
  return {
    id: requiredString(raw.id, "tokens", "id"),
    user_id: requiredString(raw.user_id, "tokens", "user_id"),
    kind: requiredString(raw.kind, "tokens", "kind"),
    name: requiredString(raw.name, "tokens", "name"),
    token_prefix: requiredString(raw.token_prefix, "tokens", "token_prefix"),
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
    limit: typeof body.limit === "number" ? body.limit : DEFAULT_LIMIT,
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

