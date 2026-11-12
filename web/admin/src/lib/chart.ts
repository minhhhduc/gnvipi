import type { RequestEvent } from "./types";

export interface Point {
  value: number | null;
}

function stepFor(raw: number): number {
  const mag = 10 ** Math.floor(Math.log10(raw));
  const n = raw / mag;
  const mult = n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10;
  return mult * mag;
}

/** Ticks from 0 to a "nice" maximum that is always >= dataMax. */
export function niceTicks(dataMax: number, target = 4): number[] {
  const max = Number.isFinite(dataMax) && dataMax > 0 ? dataMax : 1;
  let step = stepFor(max / Math.max(1, target));
  if (!Number.isFinite(step) || step <= 0) step = 1;
  let top = Math.ceil(max / step) * step;
  // One guard against floating point leaving the top tick below the data.
  while (top < max) top += step;
  const ticks: number[] = [];
  for (let v = 0; v <= top + step * 1e-6; v += step) {
    ticks.push(Math.round(v * 1e6) / 1e6);
  }
  const last = ticks[ticks.length - 1];
  if (last === undefined || last < max) ticks.push(top);
  return ticks;
}

export function axisMax(dataMax: number): number {
  const ticks = niceTicks(dataMax);
  return ticks[ticks.length - 1] || 1;
}

/**
 * Split a series into contiguous segments. Null and non-finite values are
 * gaps: no segment crosses them, so they can never be drawn as zero.
 */
export function segments(values: (number | null | undefined)[]): { index: number; value: number }[][] {
  const out: { index: number; value: number }[][] = [];
  let current: { index: number; value: number }[] = [];
  values.forEach((value, index) => {
    if (value == null || !Number.isFinite(value)) {
      if (current.length) out.push(current);
      current = [];
      return;
    }
    current.push({ index, value });
  });
  if (current.length) out.push(current);
  return out;
}

/** TTFB minutes with no streamed request are gaps, never zeros. */
export function ttfbValues(series: { ttfb_avg_ms: number }[]): (number | null)[] {
  return series.map((m) => (m.ttfb_avg_ms > 0 ? m.ttfb_avg_ms : null));
}

/** Ordered numbers from a stats series. */
export function seriesValues(series: { requests: number }[]): number[] {
  return series.map((m) => m.requests);
}

export function modelSetKey(seriesByModel: Record<string, unknown>): string {
  return JSON.stringify(Object.keys(seriesByModel).sort());
}

export function topModelNames(
  seriesByModel: Record<string, { requests: number }[]>,
  limit = 5,
): string[] {
  return Object.entries(seriesByModel)
    .map(([model, series]) => ({ model, total: series.reduce((sum, minute) => sum + minute.requests, 0) }))
    .sort((a, b) => b.total - a.total || a.model.localeCompare(b.model))
    .slice(0, limit)
    .map(({ model }) => model);
}

export type RequestSortKey =
  | "time"
  | "model"
  | "streamed"
  | "input_tokens"
  | "output_tokens"
  | "ms"
  | "ttfb_ms"
  | "error";

export type SortDirection = "asc" | "desc";

function requestSortValue(event: RequestEvent, key: RequestSortKey): string | number {
  switch (key) {
    case "time": {
      const time = Date.parse(event.time);
      return Number.isFinite(time) ? time : 0;
    }
    case "model":
      return event.model;
    case "streamed":
      return event.streamed ? 1 : 0;
    case "error":
      return event.error ? 1 : 0;
    default:
      return event[key];
  }
}

export function sortRequestEvents(
  events: RequestEvent[],
  key: RequestSortKey,
  direction: SortDirection,
): RequestEvent[] {
  const factor = direction === "asc" ? 1 : -1;
  return events.slice().sort((a, b) => {
    const aValue = requestSortValue(a, key);
    const bValue = requestSortValue(b, key);
    const comparison =
      typeof aValue === "string"
        ? aValue.localeCompare(bValue as string)
        : aValue - (bValue as number);
    return comparison * factor;
  });
}
