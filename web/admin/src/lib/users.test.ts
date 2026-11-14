import { describe, expect, it } from "vitest";
import {
  canChangeRole,
  canManageUsers,
  parseUserList,
  passwordResetBody,
  roleError,
  userPickerNote,
  validateNewPassword,
  validateNewUser,
} from "./users";

const user = {
  id: "user-1",
  username: "operator",
  email: "operator@example.test",
  display_name: "Operator",
  role: "admin",
  status: "active",
  must_change_password: false,
  failed_attempts: 0,
  locked_until: null,
  last_login_at: "2026-05-02T09:00:00Z",
  created_at: "2026-04-01T09:00:00Z",
  updated_at: "2026-05-02T09:00:00Z",
  created_by: "owner-1",
  password_hash: "sha256:super-secret",
};

describe("user list parsing", () => {
  it("whitelists display fields and never carries a password hash", () => {
    const parsed = parseUserList({ users: [user], total: 1, limit: 50, offset: 0 });
    expect(parsed.total).toBe(1);
    expect(parsed.limit).toBe(50);
    expect(parsed.offset).toBe(0);
    expect(parsed.users).toHaveLength(1);
    const record = parsed.users[0] as unknown as Record<string, unknown>;
    expect(record.password_hash).toBeUndefined();
    expect(record.username).toBe("operator");
    expect(record.created_by).toBe("owner-1");
    expect(JSON.stringify(parsed)).not.toContain("super-secret");
  });

  it("normalizes nullable timestamps and preserves values the console does not know", () => {
    const parsed = parseUserList({
      users: [{ ...user, role: "superuser", status: "suspended", locked_until: null, last_login_at: null }],
      total: 1,
    });
    expect(parsed.users[0].role).toBe("superuser");
    expect(parsed.users[0].status).toBe("suspended");
    expect(parsed.users[0].locked_until).toBeNull();
    expect(parsed.users[0].last_login_at).toBeNull();
  });

  it("rejects a malformed list body", () => {
    expect(() => parseUserList({ users: {} })).toThrow("Invalid users response");
    expect(() => parseUserList(null)).toThrow("Invalid users response");
  });
});

describe("owner gating", () => {
  it("lets only owners manage accounts", () => {
    expect(canManageUsers("owner")).toBe(true);
    expect(canManageUsers("admin")).toBe(false);
    expect(canManageUsers("user")).toBe(false);
  });
});

describe("new user validation", () => {
  const valid = {
    username: "new-user",
    display_name: "New User",
    email: "new@example.test",
    role: "user" as const,
    password: "correct-horse-battery",
  };

  it("accepts a complete form", () => {
    expect(validateNewUser(valid)).toBeNull();
  });

  it.each([
    [{ username: "ab" }, "username", "Username must be 3–64 characters"],
    [{ username: "x".repeat(65) }, "username", "Username must be 3–64 characters"],
    [{ username: "   " }, "username", "Username is required"],
    [{ password: "short" }, "password", "Password must be 12–128 characters"],
    [{ password: "x".repeat(129) }, "password", "Password must be 12–128 characters"],
    [{ role: "root" }, "role", "Role must be owner, admin or user"],
    [{ display_name: "x".repeat(201) }, "display_name", "Display name must be at most 200 characters"],
  ])("rejects %j on %s", (patch, field, message) => {
    expect(validateNewUser({ ...valid, ...patch })).toEqual({ field, message });
  });

  it("counts code points like the server's rune count", () => {
    expect(validateNewUser({ ...valid, username: "😀".repeat(40) })).toBeNull();
    expect(validateNewUser({ ...valid, display_name: "😀".repeat(200) })).toBeNull();
  });
});

describe("edit safeguards", () => {
  it("blocks a save until an unknown role is resolved", () => {
    expect(roleError("admin")).toBeNull();
    expect(roleError("superuser")).toEqual({ field: "role", message: "Role must be owner, admin or user" });
  });

  it("never edits the signed-in account's own role", () => {
    expect(canChangeRole("user-1", "user-1")).toBe(false);
    expect(canChangeRole("user-2", "user-1")).toBe(true);
  });

  it("sends the current password on the self reset path", () => {
    expect(passwordResetBody({ self: true, password: "correct-horse-battery", currentPassword: "old-secret" })).toEqual({
      password: "correct-horse-battery",
      current_password: "old-secret",
    });
    expect(passwordResetBody({ self: false, password: "correct-horse-battery" })).toEqual({
      password: "correct-horse-battery",
    });
  });

  it("states the account picker ceiling once its page fills", () => {
    expect(userPickerNote(3)).toBeNull();
    expect(userPickerNote(200)).toContain("200");
  });
});

describe("password reset validation", () => {
  it("matches the login policy and requires confirmation", () => {
    expect(validateNewPassword("short", "short")).toBe("Password must be 12–128 characters");
    expect(validateNewPassword("correct-horse-battery", "different-horse")).toBe("Passwords do not match");
    expect(validateNewPassword("correct-horse-battery", "correct-horse-battery")).toBeNull();
    expect(validateNewPassword("😀".repeat(70), "😀".repeat(70))).toBeNull();
  });
});