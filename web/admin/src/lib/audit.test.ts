import { toChildArray, type ComponentChildren, type VNode } from "preact";
import { describe, expect, it } from "vitest";
import { ApiError } from "./api";
import { message } from "./admin";
import { isOwnerRole } from "./auth";
import { pageAllowed, pageFromUrl } from "./router";
import {
  auditMatches,
  auditPath,
  auditTime,
  detailRows,
  fetchAuditPage,
  firstAuditRequest,
  nextAuditRequest,
  parseAuditPage,
  previousAuditRequest,
  sanitizeDetails,
} from "./audit";
import { AuditDetails } from "../views/Audit";

function textContent(value: ComponentChildren): string {
  return toChildArray(value)
    .map((child) => {
      if (typeof child === "string" || typeof child === "number") return String(child);
      if (!child || typeof child === "boolean") return "";
      return textContent((child as VNode<{ children?: ComponentChildren }>).props.children);
    })
    .join(" ");
}

const entryJSON = {
  id: 7,
  ts: "2026-11-14T10:20:30.123456789Z",
  actor_type: "user",
  actor_id: "11111111-1111-1111-1111-111111111111",
  user_id: "11111111-1111-1111-1111-111111111111",
  key_id: "",
  provider_id: "anthropic",
  request_id: "",
  action: "user.create",
  target_type: "user",
  target_id: "22222222-2222-2222-2222-222222222222",
  ip: "127.0.0.1",
  details: { username: "alice", role: "owner" },
};

const CANARY = "canary-super-secret-1f4a9";

describe("audit role gate", () => {
  it("opens only for an exact owner role", () => {
    expect(isOwnerRole("owner")).toBe(true);
    for (const role of ["admin", "user", "Owner", "OWNER", "toString", "__proto__", ""]) {
      expect(isOwnerRole(role)).toBe(false);
    }

    expect(pageAllowed("audit", "owner")).toBe(true);
    expect(pageAllowed("audit", "admin")).toBe(false);
    expect(pageAllowed("audit", "user")).toBe(false);
    expect(pageFromUrl("?page=audit", "owner")).toBe("audit");
    expect(pageFromUrl("?page=audit", "admin")).toBe("stats");
    expect(pageFromUrl("?page=audit", "user")).toBe("stats");
  });
});

describe("audit page parsing", () => {
  it("whitelists the entry fields and normalizes empty optionals", () => {
    const page = parseAuditPage({
      entries: [entryJSON],
      has_more: true,
      limit: 50,
      next_cursor: "next",
      before_cursor: "before",
      ignored: "x",
    });

    expect(page).toEqual({
      entries: [
        {
          id: 7,
          ts: "2026-11-14T10:20:30.123456789Z",
          actor_type: "user",
          actor_id: "11111111-1111-1111-1111-111111111111",
          user_id: "11111111-1111-1111-1111-111111111111",
          key_id: null,
          provider_id: "anthropic",
          request_id: null,
          action: "user.create",
          target_type: "user",
          target_id: "22222222-2222-2222-2222-222222222222",
          ip: "127.0.0.1",
          details: { username: "alice", role: "owner" },
        },
      ],
      has_more: true,
      limit: 50,
      next_cursor: "next",
      before_cursor: "before",
    });
    expect((page as unknown as Record<string, unknown>).ignored).toBeUndefined();
  });

  it("renders a readable timestamp and falls back on bad input", () => {
    expect(auditTime(entryJSON.ts)).toMatch(/2026/);
    expect(auditTime("not-a-date")).toBe("—");
  });

  it("filters the loaded page by action, actor, target or details", () => {
    const entry = parseAuditPage({ entries: [entryJSON], has_more: false, limit: 50 }).entries[0];
    expect(auditMatches(entry, "")).toBe(true);
    expect(auditMatches(entry, "user.create")).toBe(true);
    expect(auditMatches(entry, "alice")).toBe(true);
    expect(auditMatches(entry, "127.0.0.1")).toBe(true);
    expect(auditMatches(entry, "nope")).toBe(false);
  });
});

describe("audit redaction", () => {
  it("drops unknown detail keys, nested values and top-level secret fields", () => {
    const parsed = parseAuditPage({
      entries: [
        {
          ...entryJSON,
          secret: CANARY,
          password_hash: CANARY,
          details: {
            username: "alice",
            role: "owner",
            secret: CANARY,
            password: CANARY,
            api_key: CANARY,
            nested: { token: CANARY },
            tags: [CANARY],
          },
        },
      ],
      has_more: false,
      limit: 50,
    });

    expect(JSON.stringify(parsed)).not.toContain(CANARY);

    const entry = parsed.entries[0];
    expect(detailRows(entry)).toEqual([
      { key: "username", value: "alice" },
      { key: "role", value: "owner" },
    ]);

    const rendered = textContent(AuditDetails({ entry }));
    expect(rendered).not.toContain(CANARY);
    expect(rendered).toContain("alice");
    expect(rendered).toContain("owner");
  });

  it("keeps only allowlisted scalar details", () => {
    expect(
      sanitizeDetails({
        id: "abc",
        count: 3,
        enabled: true,
        ratio: Number.NaN,
        nested: { id: "abc" },
        token: "secret",
        NAME: "bob",
      }),
    ).toEqual({ id: "abc", count: 3, enabled: true, NAME: "bob" });
  });
});

describe("audit cursor pagination", () => {
  const firstPageBody = {
    entries: [entryJSON, { ...entryJSON, id: 6 }],
    has_more: true,
    limit: 2,
    next_cursor: "cursor-4",
    before_cursor: "cursor-7",
  };
  const secondPageBody = {
    entries: [{ ...entryJSON, id: 5 }, { ...entryJSON, id: 4 }],
    has_more: true,
    limit: 2,
    next_cursor: "cursor-2",
    before_cursor: "cursor-5",
  };

  it("pages forward with after and backward with before", () => {
    const first = parseAuditPage(firstPageBody);
    const second = parseAuditPage(secondPageBody);
    const firstRequest = firstAuditRequest(2);

    expect(auditPath(firstRequest)).toBe("/admin/api/audit?limit=2");

    const forward = nextAuditRequest(firstRequest, first);
    expect(forward).toEqual({ limit: 2, after: "cursor-4", before: null, depth: 2 });
    expect(auditPath(forward!)).toBe("/admin/api/audit?limit=2&after=cursor-4");

    const backward = previousAuditRequest(forward!, second);
    expect(backward).toEqual({ limit: 2, after: null, before: "cursor-5", depth: 1 });
    expect(auditPath(backward!)).toBe("/admin/api/audit?limit=2&before=cursor-5");
  });

  it("stops at the oldest page and at the newest page", () => {
    const first = parseAuditPage({ ...firstPageBody, has_more: false });
    const firstRequest = firstAuditRequest(2);
    expect(nextAuditRequest(firstRequest, first)).toBeNull();
    expect(previousAuditRequest(firstRequest, first)).toBeNull();
  });

  it("refuses to send both cursors", () => {
    expect(() => auditPath({ limit: 2, after: "a", before: "b" })).toThrow("pass either after or before, not both");
  });

  it("loads through the shared fetcher", async () => {
    let requested = "";
    const page = await fetchAuditPage({ limit: 2, after: "cursor-4" }, async (path) => {
      requested = path;
      return firstPageBody;
    });
    expect(requested).toBe("/admin/api/audit?limit=2&after=cursor-4");
    expect(page.entries.map((entry) => entry.id)).toEqual([7, 6]);
  });

  it("surfaces an invalid cursor as an error instead of an empty page", async () => {
    const err = new ApiError(
      "after must be a cursor returned by this endpoint",
      400,
      { error: "after must be a cursor returned by this endpoint", field: "after" },
    );
    await expect(
      fetchAuditPage({ limit: 50, after: "not-a-cursor" }, async () => {
        throw err;
      }),
    ).rejects.toBe(err);
    expect(message(err)).toBe("after: after must be a cursor returned by this endpoint");
  });
});

describe("audit empty states", () => {
  it("parses an empty page without cursors", () => {
    const page = parseAuditPage({ entries: [], has_more: false, limit: 50 });
    expect(page.entries).toEqual([]);
    expect(page.has_more).toBe(false);
    expect(page.next_cursor).toBeNull();
    expect(page.before_cursor).toBeNull();
  });

  it("rejects malformed bodies so the screen can show an error", () => {
    expect(() => parseAuditPage({ entries: "nope" })).toThrow("Invalid audit response");
    expect(() => parseAuditPage(null)).toThrow("Invalid audit response");
    expect(() => parseAuditPage({ entries: [{ id: "not-a-number" }] })).toThrow("Invalid audit response");
  });
});
