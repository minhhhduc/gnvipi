import { describe, expect, it } from "vitest";
import { shouldApplyChromeLoad } from "../views/Chromes";

describe("Chrome status request sequencing", () => {
  it("accepts only the current, non-aborted response", () => {
    expect(shouldApplyChromeLoad(4, 4)).toBe(true);
    expect(shouldApplyChromeLoad(3, 4)).toBe(false);

    const controller = new AbortController();
    controller.abort();
    expect(shouldApplyChromeLoad(4, 4, controller.signal)).toBe(false);
  });
});
