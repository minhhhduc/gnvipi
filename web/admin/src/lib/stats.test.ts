import { describe, expect, it } from "vitest";
import { selectStatsEvents } from "../views/Stats";
import type { RequestEvent } from "./types";

function event(ms: number): RequestEvent {
  return {
    time: "2026-11-13T00:00:00Z",
    model: "model",
    input_tokens: 1,
    output_tokens: 1,
    ms,
    streamed: true,
    ttfb_ms: ms / 2,
  };
}

describe("stats event selection", () => {
  it("keeps live events for KPIs while a frame supplies the request log", () => {
    const live = [event(100), event(200)];
    const frame = [event(900)];

    expect(selectStatsEvents(live, frame)).toEqual({
      kpiEvents: live,
      logEvents: frame,
    });
    expect(selectStatsEvents(live, undefined)).toEqual({
      kpiEvents: live,
      logEvents: live,
    });
  });
});
