import { describe, expect, it } from "vitest";
import type { RequestEvent } from "./types";
import { axisMax, modelSetKey, niceTicks, segments, sortRequestEvents, topModelNames, ttfbValues } from "./chart";
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

describe("topModelNames", () => {
  it("keeps its memo key stable while only request totals change", () => {
    const first = {
      alpha: [{ requests: 4 }],
      beta: [{ requests: 7 }],
      gamma: [{ requests: 1 }],
    };
    const nextPoll = {
      gamma: [{ requests: 100 }],
      beta: [{ requests: 2 }],
      alpha: [{ requests: 20 }],
    };

    expect(topModelNames(first)).toEqual(["beta", "alpha", "gamma"]);
    expect(modelSetKey(nextPoll)).toBe(modelSetKey(first));
    expect(modelSetKey({ ...nextPoll, delta: [{ requests: 1 }] })).not.toBe(modelSetKey(first));
  });

  it("uses deterministic order for tied totals and limits the legend to five models", () => {
    const byModel = Object.fromEntries(
      ["foxtrot", "echo", "delta", "charlie", "bravo", "alpha"].map((model) => [model, [{ requests: 1 }]]),
    );

    expect(topModelNames(byModel)).toEqual(["alpha", "bravo", "charlie", "delta", "echo"]);
  });
});

describe("sortRequestEvents", () => {
  const earlierOk: RequestEvent = {
    time: "2026-01-01T00:00:00Z",
    model: "alpha",
    streamed: false,
    input_tokens: 1,
    output_tokens: 10,
    ms: 100,
    ttfb_ms: 0,
  };
  const laterError: RequestEvent = {
    time: "2026-01-01T00:01:00Z",
    model: "zeta",
    streamed: true,
    input_tokens: 20,
    output_tokens: 2,
    ms: 300,
    ttfb_ms: 50,
    error: "upstream failed",
  };

  it.each([
    ["time", earlierOk, laterError],
    ["model", earlierOk, laterError],
    ["streamed", earlierOk, laterError],
    ["input_tokens", earlierOk, laterError],
    ["output_tokens", laterError, earlierOk],
    ["ms", earlierOk, laterError],
    ["ttfb_ms", earlierOk, laterError],
    ["error", earlierOk, laterError],
  ] as const)("sorts %s in both directions without mutating the source", (key, ascendingFirst, descendingFirst) => {
    const source = [laterError, earlierOk];

    expect(sortRequestEvents(source, key, "asc")).toEqual([ascendingFirst, descendingFirst]);
    expect(sortRequestEvents(source, key, "desc")).toEqual([descendingFirst, ascendingFirst]);
    expect(source).toEqual([laterError, earlierOk]);
  });
});

describe("modelPath", () => {
  it("keeps slashes literal and encodes each segment", () => {
    expect(modelPath("anthropic/claude opus")).toBe("anthropic/claude%20opus");
    expect(modelPath("a/b/c")).toBe("a/b/c");
  });
});
