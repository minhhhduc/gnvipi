import { describe, expect, it } from "vitest";
import { axisMax, niceTicks, segments, ttfbValues } from "./chart";
import { modelPath } from "./api";

describe("niceTicks", () => {
  it("rounds the axis maximum up to the next tick", () => {
    for (const max of [1, 2, 7, 14, 19, 23, 78, 184]) {
      const ticks = niceTicks(max);
      const last = ticks[ticks.length - 1];
      expect(last, `axis for ${max}`).toBeGreaterThanOrEqual(max);
      expect(axisMax(max)).toBe(last);
      expect(new Set(ticks).size).toBe(ticks.length);
      expect(ticks[0]).toBe(0);
    }
  });

  it("keeps a usable axis for zero and tiny data", () => {
    expect(niceTicks(0).at(-1)).toBeGreaterThanOrEqual(1);
    const tiny = niceTicks(2);
    expect(tiny.at(-1)).toBeGreaterThanOrEqual(2);
    expect(new Set(tiny).size).toBe(tiny.length);
    expect(tiny.length).toBeGreaterThanOrEqual(3);
  });
});

describe("segments", () => {
  it("breaks the line at null instead of drawing through zero", () => {
    const parts = segments([0, null, 100, 120]);
    expect(parts).toHaveLength(2);
    expect(parts[0]).toEqual([{ index: 0, value: 0 }]);
    expect(parts[1]).toEqual([
      { index: 2, value: 100 },
      { index: 3, value: 120 },
    ]);
  });

  it("treats a leading gap as a separate segment", () => {
    const parts = segments([null, null, 95, null, 120]);
    expect(parts.map((p) => p.map((x) => x.index))).toEqual([[2], [4]]);
  });
});

describe("ttfbValues", () => {
  it("maps zero TTFB minutes to gaps, never zeros", () => {
    const values = ttfbValues([
      { ttfb_avg_ms: 0 },
      { ttfb_avg_ms: 120 },
      { ttfb_avg_ms: 0 },
    ]);
    expect(values).toEqual([null, 120, null]);
  });
});

describe("modelPath", () => {
  it("keeps slashes literal and encodes each segment", () => {
    expect(modelPath("anthropic/claude opus")).toBe("anthropic/claude%20opus");
    expect(modelPath("a/b/c")).toBe("a/b/c");
  });
});
