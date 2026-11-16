import { isSuperuserLevelRole, type AdminUser } from "./auth";
import type { KeyModel, ProviderKey, ProviderModel } from "./types";

export type KeyReach = "full" | "disable" | "none";

export const AUTH_SCHEMES = ["bearer", "header", "basic", "none"] as const;

function optionalString(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function optionalNumber(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

function stringList(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : [];
}

function numberList(value: unknown): number[] {
  return Array.isArray(value) ? value.filter((item): item is number => typeof item === "number" && Number.isFinite(item)) : [];
}

function requiredString(value: unknown, field: string): string {
  if (typeof value !== "string" || !value) throw new Error(`Invalid keys response: ${field}`);
  return value;
}

/**
 * Only the fields the screens render cross into the UI. Writes are separate, so
 * a plaintext secret (which the server never returns anyway) cannot survive this.
 */
function keyRecord(value: unknown): ProviderKey {
  if (!value || typeof value !== "object") throw new Error("Invalid keys response: key");
  const raw = value as Record<string, unknown>;
  return {
    id: requiredString(raw.id, "id"),
    provider_id: requiredString(raw.provider_id, "provider_id"),
    owner_id: typeof raw.owner_id === "string" && raw.owner_id ? raw.owner_id : null,
    name: requiredString(raw.name, "name"),
    enabled: raw.enabled === true,
    auth_scheme: optionalString(raw.auth_scheme) || "none",
    auth_header_name: optionalString(raw.auth_header_name),
    has_secret: raw.has_secret === true,
    secret_last4: typeof raw.secret_last4 === "string" && raw.secret_last4 ? raw.secret_last4 : null,
    has_key_env: raw.has_key_env === true,
    has_key_ref: raw.has_key_ref === true,
    key_env: optionalString(raw.key_env),
    key_ref: optionalString(raw.key_ref),
    created_at: typeof raw.created_at === "string" ? raw.created_at : null,
    updated_at: typeof raw.updated_at === "string" ? raw.updated_at : null,
  };
}

export function parseKeyList(value: unknown): ProviderKey[] {
  if (!value || typeof value !== "object") throw new Error("Invalid keys response");
  const body = value as Record<string, unknown>;
  if (!Array.isArray(body.keys)) throw new Error("Invalid keys response");
  return body.keys.map(keyRecord);
}

export function parseKeyModels(value: unknown): KeyModel[] {
  if (!value || typeof value !== "object") throw new Error("Invalid key models response");
  const body = value as Record<string, unknown>;
  if (!Array.isArray(body.models)) throw new Error("Invalid key models response");
  return body.models.map((item) => {
    if (!item || typeof item !== "object") throw new Error("Invalid key models response");
    const raw = item as Record<string, unknown>;
    return {
      alias: requiredString(raw.alias, "alias"),
      upstream_model: optionalString(raw.upstream_model),
      hidden: raw.hidden === true,
      fallbacks: stringList(raw.fallbacks),
      fallback_statuses: numberList(raw.fallback_statuses),
      allow_shadow: raw.allow_shadow === true,
      legacy_position: optionalNumber(raw.legacy_position),
    };
  });
}

/** What the UI may say about a key's credential: presence and last four only. */
export function keyCredentialLabel(key: ProviderKey): string {
  if (key.has_secret) return key.secret_last4 ? `•••• ${key.secret_last4}` : "Key set";
  return "No key";
}

export function keyOwnerLabel(user: AdminUser, key: ProviderKey): string {
  if (!key.owner_id) return "shared";
  return key.owner_id === user.id ? "you" : key.owner_id;
}

/**
 * Per-row reach, mirroring the server: a superuser reaches every key, a key's
 * owner reaches it fully, an admin reaches a foreign key only to disable it.
 */
export function keyReach(user: AdminUser, key: ProviderKey): KeyReach {
  if (isSuperuserLevelRole(user.role)) return "full";
  if (key.owner_id && key.owner_id === user.id) return "full";
  return user.role === "admin" ? "disable" : "none";
}

export function canManageProviders(role: string): boolean {
  return isSuperuserLevelRole(role) || role === "admin";
}

export function canEditKey(reach: KeyReach): boolean {
  return reach === "full";
}

export function canTestKey(reach: KeyReach): boolean {
  return reach === "full";
}

export function canDeleteKey(reach: KeyReach): boolean {
  return reach === "full";
}

export function canEditKeyModels(reach: KeyReach): boolean {
  return reach === "full";
}

export function canToggleKey(reach: KeyReach): boolean {
  return reach === "full" || reach === "disable";
}

export function keyNameError(name: string): string | null {
  const runes = [...name.trim()].length;
  if (!runes) return "Key name is required";
  if (runes > 100) return "Key name must be at most 100 characters";
  return null;
}

export interface KeyFormInput {
  name: string;
  enabled: boolean;
  authScheme: string;
  authHeaderName?: string;
  secret?: string;
  keyEnv?: string;
  keyRef?: string;
  /** key_ref is owner/admin-only on the server; users never send it. */
  allowKeyRef?: boolean;
}

function keyPayload(input: KeyFormInput): Record<string, unknown> {
  const trimmed = input.name.trim();
  const error = keyNameError(trimmed);
  if (error) throw new Error(error);
  const payload: Record<string, unknown> = {
    name: trimmed,
    enabled: input.enabled,
    auth_scheme: input.authScheme,
  };
  if (input.authScheme === "none") return payload;
  if (input.authScheme === "header") {
    const header = (input.authHeaderName ?? "").trim();
    if (!header) throw new Error("Header name is required for header auth");
    payload.auth_header_name = header;
  }
  const keyEnv = input.keyEnv?.trim() ?? "";
  const keyRef = input.allowKeyRef ? input.keyRef?.trim() ?? "" : "";
  if (keyEnv && keyRef) throw new Error("Use either key_env or key_ref, not both");
  if (input.secret) payload.secret = input.secret;
  if (keyEnv) payload.key_env = keyEnv;
  if (keyRef) payload.key_ref = keyRef;
  return payload;
}

/** Create body. A shared key (`owner_id: null`) is superuser-only on the server. */
export function keyCreatePayload(input: KeyFormInput, options: { shared?: boolean } = {}): Record<string, unknown> {
  const payload = keyPayload(input);
  if (options.shared) payload.owner_id = null;
  return payload;
}

/** Update body: ownership is immutable and a blank secret keeps the stored one. */
export function keyUpdatePayload(input: KeyFormInput): Record<string, unknown> {
  return keyPayload(input);
}

export function keyDisablePayload(): { enabled: false } {
  return { enabled: false };
}

function upstreamFor(alias: string, upstream: string): string {
  const value = upstream.trim();
  if (value) return value;
  return alias.endsWith("/*") ? "" : alias;
}

function newKeyModel(alias: string, upstream: string): KeyModel {
  return {
    alias,
    upstream_model: upstreamFor(alias, upstream),
    hidden: false,
    fallbacks: [],
    fallback_statuses: [],
    allow_shadow: false,
    legacy_position: 0,
  };
}

/** Add or remove one alias on a key while preserving the rest of its settings. */
export function toggleKeyModel(
  current: KeyModel[],
  choice: { alias: string; upstream_model: string },
  selected: boolean,
): KeyModel[] {
  const alias = choice.alias.trim();
  if (!alias) return current;
  if (!selected) return current.filter((item) => item.alias !== alias);
  if (current.some((item) => item.alias === alias)) return current;
  return [...current, newKeyModel(alias, choice.upstream_model)];
}

/** Provider aliases a key does not carry yet, deduplicated, for one-click adding. */
export function modelChoices(
  providerModels: ProviderModel[] | undefined,
  keyModels: KeyModel[],
): { alias: string; upstream_model: string }[] {
  const taken = new Set(keyModels.map((item) => item.alias));
  const seen = new Set<string>();
  const out: { alias: string; upstream_model: string }[] = [];
  for (const item of providerModels ?? []) {
    const alias = (item.alias || "").trim();
    if (!alias || taken.has(alias) || seen.has(alias)) continue;
    seen.add(alias);
    out.push({ alias, upstream_model: item.upstream_model || alias });
  }
  return out;
}

export function keyModelsPayload(models: KeyModel[]): { models: KeyModel[] } {
  return {
    models: models.map((item) => ({
      alias: item.alias.trim(),
      upstream_model: upstreamFor(item.alias.trim(), item.upstream_model ?? ""),
      hidden: !!item.hidden,
      fallbacks: item.fallbacks ?? [],
      fallback_statuses: item.fallback_statuses ?? [],
      allow_shadow: !!item.allow_shadow,
      legacy_position: item.legacy_position ?? 0,
    })),
  };
}

export async function copyBaseUrl(value: string, write?: (text: string) => Promise<void>): Promise<boolean> {
  if (!value) return false;
  try {
    await (write ?? ((text: string) => navigator.clipboard.writeText(text)))(value);
    return true;
  } catch {
    return false;
  }
}
