/** Raised as the abort reason when a polled request outlives its timeout. */
export class PollTimeoutError extends Error {
  constructor() {
    super("Request timed out");
    this.name = "PollTimeoutError";
  }
}

export interface PollerOptions {
  /** Delay between a settled request and the next one. */
  intervalMs: number;
  /** Abort a request that stays in flight this long so polling cannot stall. */
  timeoutMs: number;
}

export interface Poller {
  /** Start polling; requests immediately. No-op when already running. */
  start(): void;
  /** Stop polling and abort the active request. */
  stop(): void;
  /** Request now unless one is already in flight. Returns whether one started. */
  refresh(): boolean;
}

/**
 * Single-flight poller. At most one request is in flight and the next tick is
 * scheduled after it settles, so a gateway slower than the interval never has
 * its own request aborted. Only `stop()` and the timeout abort a request.
 */
export function createPoller(
  run: (signal: AbortSignal) => Promise<unknown>,
  { intervalMs, timeoutMs }: PollerOptions,
): Poller {
  let running = false;
  let timer: number | undefined;
  let active: { controller: AbortController; timeout: number } | null = null;

  const clearTimer = () => {
    if (timer !== undefined) {
      clearTimeout(timer);
      timer = undefined;
    }
  };

  const schedule = () => {
    clearTimer();
    if (!running) return;
    timer = setTimeout(() => {
      timer = undefined;
      void tick();
    }, intervalMs);
  };

  const tick = async (): Promise<void> => {
    if (!running || active) return;
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(new PollTimeoutError()), timeoutMs);
    active = { controller, timeout };
    try {
      await run(controller.signal);
    } catch {
      /* the request owner reports its own failures */
    } finally {
      clearTimeout(timeout);
      if (active?.controller === controller) active = null;
      schedule();
    }
  };

  return {
    start() {
      if (running) return;
      running = true;
      void tick();
    },
    stop() {
      running = false;
      clearTimer();
      if (!active) return;
      const { controller, timeout } = active;
      active = null;
      clearTimeout(timeout);
      controller.abort();
    },
    refresh() {
      if (!running || active) return false;
      clearTimer();
      void tick();
      return true;
    },
  };
}