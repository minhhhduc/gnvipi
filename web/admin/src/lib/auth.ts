export type AdminRole = "owner" | "admin" | "user";

export interface AdminUser {
  id: string;
  username: string;
  email: string;
  display_name: string;
  role: AdminRole;
  status: "active" | "disabled";
  must_change_password: boolean;
}

export interface AuthSession {
  user: AdminUser;
  csrf_token: string;
}

export function isAdminRole(value: unknown): value is AdminRole {
  return value === "owner" || value === "admin" || value === "user";
}

export function isOperatorRole(value: unknown): boolean {
  return value === "owner" || value === "admin";
}

export function parseAuthSession(value: unknown): AuthSession {
  if (!value || typeof value !== "object") throw new Error("Invalid admin session response");
  const session = value as Record<string, unknown>;
  const user = session.user;
  if (!user || typeof user !== "object") throw new Error("Invalid admin session response");
  const fields = user as Record<string, unknown>;
  if (
    typeof fields.id !== "string" || !fields.id ||
    typeof fields.username !== "string" || !fields.username ||
    typeof fields.email !== "string" ||
    typeof fields.display_name !== "string" ||
    !isAdminRole(fields.role) ||
    fields.status !== "active" ||
    typeof fields.must_change_password !== "boolean" ||
    typeof session.csrf_token !== "string" || !session.csrf_token
  ) {
    throw new Error("Invalid admin session response");
  }
  return { user: fields as unknown as AdminUser, csrf_token: session.csrf_token };
}

export type AuthScreen = "login" | "password-change" | "console";

export function authScreenFor(user: AdminUser | null): AuthScreen {
  if (!user) return "login";
  return user.must_change_password ? "password-change" : "console";
}

export function passwordChangeError(
  currentPassword: string,
  newPassword: string,
  confirmation: string,
): string | null {
  if (!currentPassword) return "Current password is required";
  if (!newPassword) return "New password is required";
  if (newPassword.length < 12 || newPassword.length > 128) return "New password must be 12–128 characters";
  if (newPassword !== confirmation) return "New passwords do not match";
  return null;
}

export type SessionFailureMode = "login" | "legacy" | "error";

export function sessionFailureMode(status: number): SessionFailureMode {
  if (status === 401) return "login";
  if (status === 404) return "legacy";
  return "error";
}
