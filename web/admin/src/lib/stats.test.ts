import { describe, expect, it } from "vitest";
import { selectLogEvents } from "../views/UsageLog";
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

describe("usage log event selection", () => {
  it("reads the open frame and falls back to the live ring", () => {
    const live = [event(100), event(200)];
    const frame = [event(900)];

    expect(selectLogEvents(live, frame)).toEqual(frame);
    expect(selectLogEvents(live, undefined)).toEqual(live);
    expect(selectLogEvents(undefined, undefined)).toEqual([]);
  });
});
