import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createPoller, PollTimeoutError } from "./polling";

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("stats polling", () => {
  it("never aborts a request that outlives the poll interval", async () => {
    const signals: AbortSignal[] = [];
    let release: () => void = () => {};
    const first = new Promise<void>((resolve) => {
      release = resolve;
    });
    const run = vi.fn((signal: AbortSignal) => {
      signals.push(signal);
      return signals.length === 1 ? first : Promise.resolve();
    });
    const poller = createPoller(run, { intervalMs: 2_000, timeoutMs: 15_000 });

    poller.start();
    expect(run).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(10_000);
    expect(signals[0].aborted).toBe(false);
    expect(run).toHaveBeenCalledTimes(1);

    release();
    await vi.advanceTimersByTimeAsync(2_000);
    expect(run).toHaveBeenCalledTimes(2);
    expect(signals[0].aborted).toBe(false);
  });

  it("stops a request that hangs past the timeout and keeps polling", async () => {
    const signals: AbortSignal[] = [];
    const run = vi.fn(
      (signal: AbortSignal) =>
        new Promise<void>((_resolve, reject) => {
          signals.push(signal);
          signal.addEventListener("abort", () => reject(signal.reason));
        }),
    );
    const poller = createPoller(run, { intervalMs: 2_000, timeoutMs: 5_000 });

    poller.start();
    await vi.advanceTimersByTimeAsync(4_999);
    expect(signals[0].aborted).toBe(false);

    await vi.advanceTimersByTimeAsync(1);
    expect(signals[0].aborted).toBe(true);
    expect(signals[0].reason).toBeInstanceOf(PollTimeoutError);

    await vi.advanceTimersByTimeAsync(2_000);
    expect(run).toHaveBeenCalledTimes(2);
  });

  it("keeps one request in flight so refreshes coalesce", async () => {
    const signals: AbortSignal[] = [];
    let release: () => void = () => {};
    const first = new Promise<void>((resolve) => {
      release = resolve;
    });
    const run = vi.fn((signal: AbortSignal) => {
      signals.push(signal);
      return signals.length === 1 ? first : Promise.resolve();
    });
    const poller = createPoller(run, { intervalMs: 2_000, timeoutMs: 15_000 });

    poller.start();
    expect(poller.refresh()).toBe(false);
    await vi.advanceTimersByTimeAsync(6_000);
    expect(run).toHaveBeenCalledTimes(1);

    release();
    await vi.advanceTimersByTimeAsync(0);
    expect(poller.refresh()).toBe(true);
    expect(run).toHaveBeenCalledTimes(2);
    expect(signals).toHaveLength(2);
  });

  it("aborts and stops when the identity changes", async () => {
    const signals: AbortSignal[] = [];
    const run = vi.fn(
      (signal: AbortSignal) =>
        new Promise<void>((_resolve, reject) => {
          signals.push(signal);
          signal.addEventListener("abort", () => reject(signal.reason));
        }),
    );
    const poller = createPoller(run, { intervalMs: 2_000, timeoutMs: 15_000 });

    poller.start();
    await vi.advanceTimersByTimeAsync(0);
    poller.stop();

    expect(signals[0].aborted).toBe(true);
    expect(signals[0].reason).not.toBeInstanceOf(PollTimeoutError);
    await vi.advanceTimersByTimeAsync(30_000);
    expect(run).toHaveBeenCalledTimes(1);
  });

  it("resumes after a pause without waiting for a full interval", async () => {
    const signals: AbortSignal[] = [];
    const run = vi.fn((signal: AbortSignal) => {
      signals.push(signal);
      return Promise.resolve();
    });
    const poller = createPoller(run, { intervalMs: 2_000, timeoutMs: 15_000 });

    poller.start();
    await vi.advanceTimersByTimeAsync(0);
    poller.stop();
    const settled = run.mock.calls.length;

    poller.start();
    expect(run).toHaveBeenCalledTimes(settled + 1);
  });
});