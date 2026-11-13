import { describe, expect, it } from "vitest";
import { authScreenFor, passwordChangeError, sessionFailureMode, type AdminUser } from "./auth";

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

describe("forced password form", () => {
  it("requires the current password and a matching confirmation", () => {
    expect(passwordChangeError("", "new password", "new password")).toBe("Current password is required");
    expect(passwordChangeError("current", "new password", "different")).toBe("New passwords do not match");
    expect(passwordChangeError("current", "new password", "new password")).toBeNull();
  });
});
