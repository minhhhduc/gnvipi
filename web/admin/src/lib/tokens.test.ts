import { describe, expect, it } from "vitest";
import {
  DEFAULT_LIMIT,
  MAX_LIMIT,
  clampLimit,
  clampOffset,
  dateLabel,
  pageWindow,
} from "./admin";
import {
  defaultExpiryLabel,
  emptyPageOffset,
  expiryLabel,
  lastUsedLabel,
  parseCreatedToken,
  parseTokenList,
  tokenCreatePayload,
  tokenStatus,
} from "./tokens";

describe("token page size", () => {
  it("defaults to 50 and clamps oversized pages to 200", () => {
    expect(DEFAULT_LIMIT).toBe(50);
    expect(MAX_LIMIT).toBe(200);
    expect(clampLimit(0)).toBe(50);
    expect(clampLimit(-10)).toBe(50);
    expect(clampLimit(Number.NaN)).toBe(50);
    expect(clampLimit(25)).toBe(25);
    expect(clampLimit(500)).toBe(200);
  });
});

describe("pageWindow", () => {
  it("describes a full users page from a known total", () => {
    expect(pageWindow({ limit: 50, offset: 0, count: 50, total: 150 })).toEqual({
      from: 1,
      to: 50,
      hasPrev: false,
      hasNext: true,
    });
    expect(pageWindow({ limit: 50, offset: 100, count: 50, total: 150 })).toEqual({
      from: 101,
      to: 150,
      hasPrev: true,
      hasNext: false,
    });
    expect(pageWindow({ limit: 50, offset: 150, count: 0, total: 150 })).toEqual({
      from: 0,
      to: 0,
      hasPrev: true,
      hasNext: false,
    });
  });

  it("derives hasNext from the page size when the API reports no total", () => {
    expect(pageWindow({ limit: 50, offset: 0, count: 50 })).toMatchObject({
      hasPrev: false,
      hasNext: true,
    });
    expect(pageWindow({ limit: 50, offset: 50, count: 3 })).toMatchObject({
      from: 51,
      to: 53,
      hasPrev: true,
      hasNext: false,
    });
  });

  it("clamps an offset past the last page after the result set shrinks", () => {
    expect(clampOffset({ total: 50, limit: 50, offset: 50 })).toBe(0);
    expect(clampOffset({ total: 51, limit: 50, offset: 100 })).toBe(50);
    expect(clampOffset({ total: 0, limit: 50, offset: 50 })).toBe(0);
    expect(clampOffset({ total: 120, limit: 50, offset: 100 })).toBe(100);
    expect(clampOffset({ total: 120, limit: 50, offset: 0 })).toBe(0);
  });
});

describe("token status", () => {
  const now = Date.parse("2026-06-01T12:00:00Z");

  it("reports revoked before expiry", () => {
    expect(tokenStatus({ revoked_at: "2026-05-01T00:00:00Z", expires_at: "2026-07-01T00:00:00Z" }, now)).toBe("revoked");
  });

  it("reports expired once the expiry time passes", () => {
    expect(tokenStatus({ expires_at: "2026-05-31T12:00:00Z" }, now)).toBe("expired");
    expect(tokenStatus({ expires_at: "2026-06-01T12:00:01Z" }, now)).toBe("active");
  });

  it("treats a missing expiry as never expiring", () => {
    expect(tokenStatus({}, now)).toBe("active");
    expect(expiryLabel(null)).toBe("Never");
    expect(lastUsedLabel(null)).toBe("Never used");
    expect(dateLabel(null)).toBe("—");
  });
});

describe("expiry labels", () => {
  it("renders the calendar date of an expiry", () => {
    expect(expiryLabel("2027-03-04T05:06:07Z")).toMatch(/2027/);
    expect(expiryLabel("not-a-date")).toBe("—");
  });

  it("labels the server default per token kind", () => {
    expect(defaultExpiryLabel("gateway")).toBe("365 days (default)");
    expect(defaultExpiryLabel("admin")).toBe("90 days (default)");
  });
});

describe("token create payload", () => {
  it("omits expiry fields so the gateway applies its 365-day default", () => {
    expect(tokenCreatePayload({ name: " laptop ", kind: "gateway", expiry: "default" })).toEqual({
      name: "laptop",
      kind: "gateway",
    });
  });

  it("sends no_expiry for a gateway token that never expires", () => {
    expect(tokenCreatePayload({ name: "ci", kind: "gateway", expiry: "none" })).toEqual({
      name: "ci",
      kind: "gateway",
      no_expiry: true,
    });
  });

  it("keeps admin tokens on the server expiry and carries the scope", () => {
    expect(tokenCreatePayload({ name: "cli", kind: "admin", scope: "write", expiry: "default" })).toEqual({
      name: "cli",
      kind: "admin",
      scope: "write",
    });
    expect(() => tokenCreatePayload({ name: "cli", kind: "admin", scope: "read", expiry: "none" })).toThrow(
      "Admin tokens require an expiry",
    );
  });

  it("addresses another account only when the owner names one", () => {
    expect(tokenCreatePayload({ name: "ops", kind: "gateway", expiry: "default", userId: "user-2" })).toMatchObject({
      user_id: "user-2",
    });
  });

  it("rejects an empty name instead of posting it", () => {
    expect(() => tokenCreatePayload({ name: "   ", kind: "gateway", expiry: "default" })).toThrow(
      "Token name is required",
    );
  });

  it("rejects a name over the server's 100-rune cap", () => {
    expect(() =>
      tokenCreatePayload({ name: "😀".repeat(101), kind: "gateway", expiry: "default" }),
    ).toThrow("Token name must be at most 100 characters");
    expect(tokenCreatePayload({ name: "😀".repeat(100), kind: "gateway", expiry: "default" }).name).toBe("😀".repeat(100));
  });
});

describe("empty page recovery", () => {
  it("steps back a page when an offset page comes back empty", () => {
    expect(emptyPageOffset(50, 0)).toBe(0);
    expect(emptyPageOffset(50, 50)).toBe(0);
    expect(emptyPageOffset(50, 100)).toBe(50);
    expect(emptyPageOffset(200, 200)).toBe(0);
  });
});

describe("token list parsing", () => {
  const body = {
    tokens: [
      {
        id: "token-1",
        user_id: "user-1",
        kind: "gateway",
        name: "laptop",
        token_prefix: "gwk_abcd",
        scope: null,
        expires_at: null,
        last_used_at: "2026-05-01T10:00:00Z",
        revoked_at: null,
        created_at: "2026-04-01T10:00:00Z",
        token_hash: "sha256:secret-hash",
        token: "gwk_secret-plaintext",
      },
    ],
    limit: 50,
    offset: 0,
  };

  it("keeps the display fields and never carries a secret", () => {
    const parsed = parseTokenList(body);
    expect(parsed.tokens).toHaveLength(1);
    expect(parsed.limit).toBe(50);
    expect(parsed.offset).toBe(0);
    const token = parsed.tokens[0] as unknown as Record<string, unknown>;
    expect(Object.keys(token).sort()).toEqual([
      "created_at",
      "expires_at",
      "id",
      "kind",
      "last_used_at",
      "name",
      "revoked_at",
      "scope",
      "token_prefix",
      "user_id",
    ]);
    expect(JSON.stringify(parsed)).not.toContain("secret");
  });

  it("rejects a malformed list body", () => {
    expect(() => parseTokenList({ tokens: "nope" })).toThrow("Invalid tokens response");
    expect(() => parseTokenList(null)).toThrow("Invalid tokens response");
  });
});

describe("created token parsing", () => {
  it("surfaces the plaintext exactly once from the create response", () => {
    const parsed = parseCreatedToken({
      id: "token-1",
      user_id: "user-1",
      kind: "gateway",
      name: "laptop",
      token_prefix: "gwk_abcd",
      scope: null,
      expires_at: null,
      last_used_at: null,
      revoked_at: null,
      created_at: "2026-04-01T10:00:00Z",
      token: "gwk_plaintext",
    });
    expect(parsed.token).toBe("gwk_plaintext");
    expect(parsed.record.id).toBe("token-1");
    expect(JSON.stringify(parsed.record)).not.toContain("gwk_plaintext");
  });

  it("rejects a create response without the one-time token", () => {
    expect(() => parseCreatedToken({ id: "token-1" })).toThrow("Invalid created token response");
  });
});