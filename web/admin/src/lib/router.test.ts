import { describe, expect, it } from "vitest";
import { pageFromUrl } from "./router";

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
});
