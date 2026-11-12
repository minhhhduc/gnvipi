import { describe, expect, it } from "vitest";
import {
  engineForSearchUrl,
  syncSearchEngine,
  syncSearchUrl,
} from "../views/Config";

const GOOGLE = "https://www.google.com/search?q={query}";
const CUSTOM = "https://search.example/?q={query}";

describe("config search synchronization", () => {
  it("derives the matching engine from a saved URL", () => {
    expect(engineForSearchUrl("")).toBe("");
    expect(engineForSearchUrl(GOOGLE)).toBe(GOOGLE);
    expect(engineForSearchUrl(CUSTOM)).toBe("custom");
  });

  it("marks an edited URL as custom", () => {
    expect(syncSearchUrl(GOOGLE)).toEqual({ engine: "custom", url: GOOGLE });
  });

  it("keeps the current URL when custom is selected", () => {
    expect(syncSearchEngine("custom", CUSTOM)).toEqual({
      engine: "custom",
      url: CUSTOM,
      save: false,
    });
  });

  it("persists a preset even when it is already displayed", () => {
    expect(syncSearchEngine(GOOGLE, GOOGLE)).toEqual({
      engine: GOOGLE,
      url: GOOGLE,
      save: true,
    });
  });
});
