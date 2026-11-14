import type { AdminRole } from "./auth";
import type { AdminUserRecord, UserList } from "./types";

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

const ROLES: readonly string[] = ["owner", "admin", "user"];

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
  if (username.length < 3 || username.length > 64) {
    return { field: "username", message: "Username must be 3–64 characters" };
  }
  if (input.display_name.trim().length > 200) {
    return { field: "display_name", message: "Display name must be at most 200 characters" };
  }
  if (!ROLES.includes(input.role)) {
    return { field: "role", message: "Role must be owner, admin or user" };
  }
  if (input.password.length < 12 || input.password.length > 128) {
    return { field: "password", message: "Password must be 12–128 characters" };
  }
  return null;
}

/** A reset issues a temporary secret under the same login policy. */
export function validateNewPassword(password: string, confirmation: string): string | null {
  if (password.length < 12 || password.length > 128) return "Password must be 12–128 characters";
  if (password !== confirmation) return "Passwords do not match";
  return null;
}

function optionalString(value: unknown): string | null {
  return typeof value === "string" && value ? value : null;
}

function requiredString(value: unknown, field: string): string {
  if (typeof value !== "string" || !value) throw new Error(`Invalid users response: ${field}`);
  return value;
}

/**
 * Only the fields the table renders cross into the UI. The password hash is not
 * among them, so a handler that ever widened its response still cannot leak it.
 */
function userRecord(value: unknown): AdminUserRecord {
  if (!value || typeof value !== "object") throw new Error("Invalid users response: user");
  const raw = value as Record<string, unknown>;
  const role = requiredString(raw.role, "role");
  return {
    id: requiredString(raw.id, "id"),
    username: requiredString(raw.username, "username"),
    email: optionalString(raw.email),
    display_name: optionalString(raw.display_name),
    role: ROLES.includes(role) ? (role as AdminRole) : "admin",
    status: raw.status === "disabled" ? "disabled" : "active",
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