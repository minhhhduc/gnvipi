import { describe, expect, it } from "vitest";
import {
  canManageUsers,
  parseUserList,
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

  it("normalizes nullable timestamps and unknown roles", () => {
    const parsed = parseUserList({
      users: [{ ...user, role: "superuser", locked_until: null, last_login_at: null }],
      total: 1,
    });
    expect(parsed.users[0].role).toBe("admin");
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
});

describe("password reset validation", () => {
  it("matches the login policy and requires confirmation", () => {
    expect(validateNewPassword("short", "short")).toBe("Password must be 12–128 characters");
    expect(validateNewPassword("correct-horse-battery", "different-horse")).toBe("Passwords do not match");
    expect(validateNewPassword("correct-horse-battery", "correct-horse-battery")).toBeNull();
  });
});