import { describe, expect, it } from "vitest";
import { pageAllowed, pageFromUrl, rawPageFromUrl } from "./router";
import type { Page } from "./context";

describe("pageFromUrl", () => {
  it.each([
    ["?page=stats", "stats"],
    ["?page=models", "models"],
    ["?page=config", "config"],
    ["?page=chromes", "chromes"],
    ["?page=users", "users"],
    ["?page=tokens", "tokens"],
  ] as const)("reads the direct route from %s", (search, expected) => {
    expect(pageFromUrl(search)).toBe(expected);
  });

  it.each(["", "?page=unknown", "?other=models"])(
    "falls back to stats for %s",
    (search) => {
      expect(pageFromUrl(search)).toBe("stats");
    },
  );

  it("filters direct routes through the signed-in role", () => {
    const forRole = pageFromUrl as (
      search: string,
      role: "owner" | "admin" | "user",
    ) => Page;

    expect(forRole("?page=config", "owner")).toBe("config");
    expect(forRole("?page=chromes", "admin")).toBe("chromes");
    expect(forRole("?page=models", "user")).toBe("stats");
    expect(forRole("?page=config", "user")).toBe("stats");
    expect(forRole("?page=chromes", "user")).toBe("stats");
  });

  it("keeps the users screen owner-only and the tokens screen open to every role", () => {
    const forRole = pageFromUrl as (
      search: string,
      role: "owner" | "admin" | "user",
    ) => Page;

    expect(forRole("?page=users", "owner")).toBe("users");
    expect(forRole("?page=users", "admin")).toBe("stats");
    expect(forRole("?page=users", "user")).toBe("stats");
    expect(forRole("?page=tokens", "owner")).toBe("tokens");
    expect(forRole("?page=tokens", "admin")).toBe("tokens");
    expect(forRole("?page=tokens", "user")).toBe("tokens");
  });
});

describe("rawPageFromUrl", () => {
  it("preserves an unknown page for the fallback notice", () => {
    expect(rawPageFromUrl("?page=unknown")).toBe("unknown");
    expect(rawPageFromUrl("?page=")).toBe("");
    expect(rawPageFromUrl("?other=models")).toBe("");
  });
});

describe("pageAllowed", () => {
  it("gates the owner-only users screen and the shared tokens screen", () => {
    expect(pageAllowed("users", "owner")).toBe(true);
    expect(pageAllowed("users", "admin")).toBe(false);
    expect(pageAllowed("users", "user")).toBe(false);
    expect(pageAllowed("tokens", "owner")).toBe(true);
    expect(pageAllowed("tokens", "admin")).toBe(true);
    expect(pageAllowed("tokens", "user")).toBe(true);
  });
});
