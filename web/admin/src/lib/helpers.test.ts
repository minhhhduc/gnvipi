import { describe, expect, it } from "vitest";
import {
  buildUpstreamModel,
  clampFrame,
  computeAutoModeOptions,
  computeCycleAvailableOptions,
  computeCycleUnselected,
  formatLastOk,
  getFrameNavState,
  getProtocolOptions,
  runningWorkersRatio,
} from "./helpers";

describe("buildUpstreamModel", () => {
  it("preserves upstream exactly (including empty) for wildcard aliases", () => {
    expect(buildUpstreamModel("anthropic/*", "")).toBe("");
    expect(buildUpstreamModel("anthropic/*", "   ")).toBe("");
    expect(buildUpstreamModel("vendor/*", "upstream/prefix/*")).toBe("upstream/prefix/*");
  });

  it("falls back to alias when upstream is empty for non-wildcard aliases", () => {
    expect(buildUpstreamModel("gpt-4o", "")).toBe("gpt-4o");
    expect(buildUpstreamModel("gpt-4o", "   ")).toBe("gpt-4o");
    expect(buildUpstreamModel("gpt-4o", "openai/gpt-4o")).toBe("openai/gpt-4o");
  });
});

describe("getProtocolOptions", () => {
  it("includes openai-responses alongside chat and messages", () => {
    const opts = getProtocolOptions("openai-chat");
    expect(opts).toEqual(["openai-chat", "anthropic-messages", "openai-responses"]);
  });

  it("injects custom or unknown protocol as an option without rewriting", () => {
    const opts = getProtocolOptions("custom:fast");
    expect(opts).toEqual(["custom:fast", "openai-chat", "anthropic-messages", "openai-responses"]);
  });

  it("preserves initial custom protocol even if current value switches", () => {
    const opts = getProtocolOptions("openai-chat", "custom:legacy");
    expect(opts).toEqual(["custom:legacy", "openai-chat", "anthropic-messages", "openai-responses"]);
  });
});

describe("computeAutoModeOptions", () => {
  it("excludes hidden catalog models from new selections and appends cycle", () => {
    const catalog = [
      { id: "gpt-4o", hidden: false },
      { id: "secret-model", hidden: true },
      { id: "claude-3-5-sonnet", hidden: false },
    ];
    const opts = computeAutoModeOptions(catalog);
    expect(opts).toEqual(["claude-3-5-sonnet", "gpt-4o", "cycle"]);
  });

  it("retains current target even if hidden in catalog", () => {
    const catalog = [
      { id: "gpt-4o", hidden: false },
      { id: "secret-model", hidden: true },
    ];
    const opts = computeAutoModeOptions(catalog, "secret-model");
    expect(opts).toEqual(["gpt-4o", "secret-model", "cycle"]);
  });

  it("handles cycle as current target without duplication", () => {
    const catalog = [{ id: "gpt-4o", hidden: false }];
    const opts = computeAutoModeOptions(catalog, "cycle");
    expect(opts).toEqual(["gpt-4o", "cycle"]);
  });
});

describe("computeCycleAvailableOptions & computeCycleUnselected", () => {
  it("keeps current cycle models and available models even if hidden", () => {
    const catalog = [
      { id: "model-a", hidden: false },
      { id: "model-b", hidden: true },
      { id: "model-c", hidden: true },
    ];
    const cycleModels = ["model-b"];
    const available = ["model-d"];
    const pool = computeCycleAvailableOptions(catalog, cycleModels, available);
    expect(pool).toEqual(["model-a", "model-b", "model-d"]);
  });

  it("excludes hidden catalog models from unselected list", () => {
    const catalog = [
      { id: "model-a", hidden: false },
      { id: "model-b", hidden: true },
      { id: "model-hidden-available", hidden: true },
      { id: "model-c", hidden: false },
    ];
    const cycleModels = ["model-a"];
    const available = ["model-hidden-available", "model-c"];
    const unselected = computeCycleUnselected(catalog, cycleModels, available);
    expect(unselected).toEqual(["model-c"]);
  });
});

describe("clampFrame", () => {
  it("clamps requested frame to [1, totalFrames]", () => {
    expect(clampFrame(0, 5)).toBe(1);
    expect(clampFrame(1, 5)).toBe(1);
    expect(clampFrame(3, 5)).toBe(3);
    expect(clampFrame(5, 5)).toBe(5);
    expect(clampFrame(6, 5)).toBe(5);
  });

  it("returns 1 when totalFrames is 0 or negative", () => {
    expect(clampFrame(0, 0)).toBe(1);
    expect(clampFrame(3, 0)).toBe(1);
    expect(clampFrame(-2, -1)).toBe(1);
  });
});

describe("getFrameNavState", () => {
  it("disables both prev and next when totalFrames is 0", () => {
    const state = getFrameNavState(null, 0);
    expect(state.canPrev).toBe(false);
    expect(state.canLatest).toBe(false);
    expect(state.canNext).toBe(false);
  });

  it("opens frame 1 on next from live view", () => {
    const state = getFrameNavState(null, 4);
    expect(state.canPrev).toBe(false);
    expect(state.canLatest).toBe(false);
    expect(state.canNext).toBe(true);
    expect(state.nextTarget).toBe(1);
  });

  it("disables both prev and next at frame 1 when totalFrames is 1", () => {
    const state = getFrameNavState(1, 1);
    expect(state.canPrev).toBe(false);
    expect(state.canLatest).toBe(true);
    expect(state.canNext).toBe(false);
  });

  it("pages forward and backward correctly through multiple frames", () => {
    const f1 = getFrameNavState(1, 3);
    expect(f1.canPrev).toBe(false);
    expect(f1.canNext).toBe(true);
    expect(f1.nextTarget).toBe(2);

    const f2 = getFrameNavState(2, 3);
    expect(f2.canPrev).toBe(true);
    expect(f2.prevTarget).toBe(1);
    expect(f2.canNext).toBe(true);
    expect(f2.nextTarget).toBe(3);

    const f3 = getFrameNavState(3, 3);
    expect(f3.canPrev).toBe(true);
    expect(f3.prevTarget).toBe(2);
    expect(f3.canNext).toBe(false);
  });
});

describe("runningWorkersRatio", () => {
  it("computes ratio of non-killed workers to total", () => {
    expect(runningWorkersRatio([
      { killed: false },
      { killed: true },
      { killed: false },
    ])).toBe("2 / 3");

    expect(runningWorkersRatio([])).toBe("0 / 0");
    expect(runningWorkersRatio(undefined)).toBe("0 / 0");
    expect(runningWorkersRatio([{ killed: true }])).toBe("0 / 1");
  });
});

describe("formatLastOk", () => {
  it("formats zero/epoch/empty times as never", () => {
    expect(formatLastOk("")).toBe("never");
    expect(formatLastOk(null)).toBe("never");
    expect(formatLastOk(undefined)).toBe("never");
    expect(formatLastOk("0001-01-01T00:00:00Z")).toBe("never");
    expect(formatLastOk("invalid-date")).toBe("never");
  });

  it("formats valid modern timestamps as a time string", () => {
    const formatted = formatLastOk("2026-11-12T05:30:00Z");
    expect(formatted).not.toBe("never");
    expect(formatted).toMatch(/\d{1,2}:\d{2}/);
  });
});
