import type { AdminRole } from "./auth";
import type { AdminUserRecord, UserList } from "./types";
import { optionalString, requiredString } from "./admin";

/**
 * Account administration is owner-only. The router gates the page as well; this
 * is the second check the views use before rendering any mutation control.
 */
export function canManageUsers(role: AdminRole): boolean {
  return role === "owner";
}

export interface FieldError {
  field: string;
  message: string;
}

export const ROLES: readonly string[] = ["owner", "admin", "user"];

/** The owner account picker loads one server-capped page. */
export const USER_LIST_MAX = 200;

/** The server validates lengths with utf8.RuneCountInString, so count runes. */
function runeLength(value: string): number {
  return [...value].length;
}

export function roleError(role: string): FieldError | null {
  if (ROLES.includes(role)) return null;
  return { field: "role", message: "Role must be owner, admin or user" };
}

/** The server's display-name CHECK, shared by create and edit. */
export function displayNameError(displayName: string): FieldError | null {
  if (runeLength(displayName.trim()) > 200) {
    return { field: "display_name", message: "Display name must be at most 200 characters" };
  }
  return null;
}

/** Mirrors the server's create-user checks so the operator sees them inline. */
export function validateNewUser(input: {
  username: string;
  display_name: string;
  email: string;
  role: string;
  password: string;
}): FieldError | null {
  const username = input.username.trim();
  if (!username) return { field: "username", message: "Username is required" };
  if (runeLength(username) < 3 || runeLength(username) > 64) {
    return { field: "username", message: "Username must be 3–64 characters" };
  }
  const invalidName = displayNameError(input.display_name);
  if (invalidName) return invalidName;
  const invalidRole = roleError(input.role);
  if (invalidRole) return invalidRole;
  if (runeLength(input.password) < 12 || runeLength(input.password) > 128) {
    return { field: "password", message: "Password must be 12–128 characters" };
  }
  return null;
}

/** A reset issues a temporary secret under the same login policy. */
export function validateNewPassword(password: string, confirmation: string): string | null {
  if (runeLength(password) < 12 || runeLength(password) > 128) return "Password must be 12–128 characters";
  if (password !== confirmation) return "Passwords do not match";
  return null;
}

/** The self path of the password endpoint also demands the current secret. */
export function passwordResetBody(input: {
  self: boolean;
  password: string;
  currentPassword?: string;
}): Record<string, unknown> {
  return input.self
    ? { password: input.password, current_password: input.currentPassword ?? "" }
    : { password: input.password };
}

/** Editing your own role would leave the SPA on a stale session. */
export function canChangeRole(targetID: string, viewerID: string): boolean {
  return targetID !== viewerID;
}

/** The picker cannot page; say so when its single page is full. */
export function userPickerNote(count: number): string | null {
  return count >= USER_LIST_MAX
    ? `Only the newest ${USER_LIST_MAX} accounts are listed; older accounts cannot be selected here.`
    : null;
}

/**
 * Only the fields the table renders cross into the UI. The password hash is not
 * among them, so a handler that ever widened its response still cannot leak it.
 */
function userRecord(value: unknown): AdminUserRecord {
  if (!value || typeof value !== "object") throw new Error("Invalid users response: user");
  const raw = value as Record<string, unknown>;
  return {
    id: requiredString(raw.id, "users", "id"),
    username: requiredString(raw.username, "users", "username"),
    email: optionalString(raw.email),
    display_name: optionalString(raw.display_name),
    // Unknown roles and statuses are preserved so an edit cannot silently
    // rewrite them; the view blocks a save until the role is known.
    role: requiredString(raw.role, "users", "role"),
    status: typeof raw.status === "string" && raw.status ? raw.status : "active",
    must_change_password: raw.must_change_password === true,
    failed_attempts: typeof raw.failed_attempts === "number" ? raw.failed_attempts : 0,
    locked_until: optionalString(raw.locked_until),
    last_login_at: optionalString(raw.last_login_at),
    created_at: optionalString(raw.created_at),
    updated_at: optionalString(raw.updated_at),
    created_by: optionalString(raw.created_by),
  };
}

export function parseUserList(value: unknown): UserList {
  if (!value || typeof value !== "object") throw new Error("Invalid users response");
  const body = value as Record<string, unknown>;
  if (!Array.isArray(body.users)) throw new Error("Invalid users response");
  return {
    users: body.users.map(userRecord),
    total: typeof body.total === "number" ? body.total : 0,
    limit: typeof body.limit === "number" ? body.limit : 50,
    offset: typeof body.offset === "number" ? body.offset : 0,
  };
}