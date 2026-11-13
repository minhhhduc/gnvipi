import { describe, expect, it } from "vitest";
import {
  drainLatestWrites,
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

describe("latest config writes", () => {
  it("serializes writes and saves only the newest pending target", async () => {
    const pending = { current: ["a", "b", "c"] as string[] | null };
    const inFlight = { current: false };
    const writes: string[][] = [];
    const confirmed: string[][] = [];
    let activeWrites = 0;
    let maxActiveWrites = 0;
    let releaseFirst!: () => void;

    const first = drainLatestWrites(
      pending,
      inFlight,
      async (target) => {
        writes.push(target);
        activeWrites++;
        maxActiveWrites = Math.max(maxActiveWrites, activeWrites);
        if (writes.length === 1) await new Promise<void>((resolve) => { releaseFirst = resolve; });
        activeWrites--;
      },
      (target) => confirmed.push(target),
    );
    await Promise.resolve();

    pending.current = ["b", "a", "c"];
    const superseded = drainLatestWrites(pending, inFlight, async () => {}, () => {});
    pending.current = ["b", "c", "a"];
    const latest = drainLatestWrites(pending, inFlight, async () => {}, () => {});
    releaseFirst();

    expect(await superseded).toEqual({ started: false });
    expect(await latest).toEqual({ started: false });
    expect(await first).toEqual({ started: true, ok: true });
    expect(writes).toEqual([
      ["a", "b", "c"],
      ["b", "c", "a"],
    ]);
    expect(confirmed).toEqual(writes);
    expect(maxActiveWrites).toBe(1);
  });

  it("stops after a failed write and discards newer pending targets", async () => {
    const pending = { current: ["a"] as string[] | null };
    const inFlight = { current: false };
    const writes: string[][] = [];
    const confirmed: string[][] = [];
    let releaseFirst!: () => void;
    let rejectSecond!: (error: Error) => void;
    let secondStarted!: () => void;
    const secondIsRunning = new Promise<void>((resolve) => { secondStarted = resolve; });

    const result = drainLatestWrites(
      pending,
      inFlight,
      async (target) => {
        writes.push(target);
        if (writes.length === 1) {
          await new Promise<void>((resolve) => { releaseFirst = resolve; });
        } else {
          secondStarted();
          await new Promise<void>((_resolve, reject) => { rejectSecond = reject; });
        }
      },
      (target) => confirmed.push(target),
    );
    await Promise.resolve();

    pending.current = ["b"];
    releaseFirst();
    await secondIsRunning;
    pending.current = ["c"];
    const queued = drainLatestWrites(pending, inFlight, async () => {}, () => {});
    rejectSecond(new Error("save failed"));

    expect(await queued).toEqual({ started: false });
    expect(await result).toMatchObject({ started: true, ok: false, error: new Error("save failed") });
    expect(writes).toEqual([["a"], ["b"]]);
    expect(confirmed).toEqual([["a"]]);
    expect(pending.current).toBeNull();
    expect(inFlight.current).toBe(false);
  });
});
