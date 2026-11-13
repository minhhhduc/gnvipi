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
  if (newPassword !== confirmation) return "New passwords do not match";
  return null;
}

export type SessionFailureMode = "login" | "legacy" | "error";

export function sessionFailureMode(status: number): SessionFailureMode {
  if (status === 401) return "login";
  if (status === 404) return "legacy";
  return "error";
}
