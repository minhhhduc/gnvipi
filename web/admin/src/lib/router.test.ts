import { describe, expect, it } from "vitest";
import { pageFromUrl, rawPageFromUrl } from "./router";
import type { Page } from "./context";

describe("pageFromUrl", () => {
  it.each([
    ["?page=stats", "stats"],
    ["?page=models", "models"],
    ["?page=config", "config"],
    ["?page=chromes", "chromes"],
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
});

describe("rawPageFromUrl", () => {
  it("preserves an unknown page for the fallback notice", () => {
    expect(rawPageFromUrl("?page=unknown")).toBe("unknown");
    expect(rawPageFromUrl("?page=")).toBe("");
    expect(rawPageFromUrl("?other=models")).toBe("");
  });
});
