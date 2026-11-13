import { describe, expect, it } from "vitest";
import { authScreenFor, parseAuthSession, passwordChangeError, sessionFailureMode, type AdminUser } from "./auth";

function user(mustChangePassword: boolean): AdminUser {
  return {
    id: "user-1",
    username: "operator",
    email: "operator@example.test",
    display_name: "Operator",
    role: "user",
    status: "active",
    must_change_password: mustChangePassword,
  };
}

describe("authentication screen selection", () => {
  it("shows login before a session is established", () => {
    expect(authScreenFor(null)).toBe("login");
  });

  it("forces every role through password change before the console", () => {
    expect(authScreenFor({ ...user(true), role: "owner" })).toBe("password-change");
    expect(authScreenFor({ ...user(true), role: "admin" })).toBe("password-change");
    expect(authScreenFor(user(true))).toBe("password-change");
  });

  it("keeps the transitional file-backed console while identity routes are absent", () => {
    expect(sessionFailureMode(401)).toBe("login");
    expect(sessionFailureMode(404)).toBe("legacy");
    expect(sessionFailureMode(500)).toBe("error");
  });

  it("enters the role-aware console only after the password flag clears", () => {
    expect(authScreenFor(user(false))).toBe("console");
  });
});

describe("admin session validation", () => {
  const valid = { user: user(false), csrf_token: "csrf-token" };

  it("accepts only active owner, admin, and user sessions", () => {
    expect(parseAuthSession(valid)).toEqual(valid);
    expect(parseAuthSession({ ...valid, user: { ...valid.user, role: "owner" } }).user.role).toBe("owner");
    expect(parseAuthSession({ ...valid, user: { ...valid.user, role: "admin" } }).user.role).toBe("admin");
  });

  it.each([
    { ...valid, csrf_token: "" },
    { ...valid, user: { ...valid.user, role: "viewer" } },
    { ...valid, user: { ...valid.user, status: "disabled" } },
    { ...valid, user: { ...valid.user, must_change_password: "yes" } },
    { user: null, csrf_token: "csrf-token" },
  ])("rejects a malformed or inactive session", (value) => {
    expect(() => parseAuthSession(value)).toThrow("Invalid admin session response");
  });
});

describe("forced password form", () => {
  it("requires current credentials, policy length, and matching confirmation", () => {
    expect(passwordChangeError("", "new password", "new password")).toBe("Current password is required");
    expect(passwordChangeError("current", "short", "short")).toBe("New password must be 12–128 characters");
    const tooLong = "x".repeat(129);
    expect(passwordChangeError("current", tooLong, tooLong)).toBe("New password must be 12–128 characters");
    expect(passwordChangeError("current", "new password", "different")).toBe("New passwords do not match");
    expect(passwordChangeError("current", "new password", "new password")).toBeNull();
  });
});
