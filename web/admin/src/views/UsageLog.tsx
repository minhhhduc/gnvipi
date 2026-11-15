import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { api, ApiError } from "../lib/api";
import { useGateway } from "../lib/context";
import { clock, fmtInt, fmtMs } from "../lib/format";
import { clampFrame, usagePageWindow } from "../lib/helpers";
import { sortRequestEvents } from "../lib/chart";
import type { RequestSortKey, SortDirection } from "../lib/chart";
import type { FrameResponse, RequestEvent, Stats as StatsData } from "../lib/types";
import { Card, CardHead, Empty, PageHeader, Pager } from "../components/ui";
import { HealthPill } from "../components/Shell";

const NO_EVENTS: RequestEvent[] = [];

/** The log follows the open frame; the live ring supplies the first frame. */
export function selectLogEvents(live?: RequestEvent[], frame?: RequestEvent[]): RequestEvent[] {
  return frame ?? live ?? NO_EVENTS;
}

export function UsageLogView() {
  const { stats, statsUpdated } = useGateway();
  const [frame, setFrame] = useState<number | null>(null);
  const [frameData, setFrameData] = useState<FrameResponse | null>(null);
  const [frameError, setFrameError] = useState<string | null>(null);
  const [retryAt, setRetryAt] = useState(0);
  const [eventSort, setEventSort] = useState<{ key: RequestSortKey; direction: SortDirection }>({
    key: "time",
    direction: "desc",
  });
  const [pollingPaused, setPollingPaused] = useState(() => document.hidden);
  const data: StatsData = stats || {};
  const logEvents = selectLogEvents(data.events, frameData?.events);
  const totalFrames = frameData?.total_frames ?? data.total_frames ?? 0;

  const sortedEvents = useMemo(
    () => sortRequestEvents(logEvents, eventSort.key, eventSort.direction),
    [logEvents, eventSort.key, eventSort.direction],
  );

  const frameRef = useRef<number | null>(frame);
  frameRef.current = frame;
  const lastReadAtRef = useRef<number>(0);
  const reqIdRef = useRef<number>(0);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    const onVisibilityChange = () => setPollingPaused(document.hidden);
    document.addEventListener("visibilitychange", onVisibilityChange);
    return () => document.removeEventListener("visibilitychange", onVisibilityChange);
  }, []);

  useEffect(() => {
    if (frame == null || totalFrames <= 0) {
      if (timerRef.current) clearTimeout(timerRef.current);
      setFrameData(null);
      setFrameError(null);
      setRetryAt(0);
      return;
    }

    const targetFrame = clampFrame(frame, totalFrames);
    let aborted = false;
    const ctrl = new AbortController();

    const doFetch = async () => {
      if (aborted || ctrl.signal.aborted || frameRef.current !== targetFrame) return;

      const now = Date.now();
      const elapsed = now - lastReadAtRef.current;
      const minInterval = 1000;
      if (elapsed < minInterval) {
        const wait = minInterval - elapsed;
        timerRef.current = setTimeout(() => {
          void doFetch();
        }, wait);
        return;
      }

      lastReadAtRef.current = Date.now();
      const thisReq = ++reqIdRef.current;

      try {
        const d = await api<FrameResponse>(`/admin/api/stats?frames=${targetFrame}`, { signal: ctrl.signal });
        if (aborted || ctrl.signal.aborted || reqIdRef.current !== thisReq || frameRef.current !== targetFrame) return;
        setFrameData(d);
        setFrameError(null);
        setRetryAt(0);
      } catch (err) {
        if (aborted || ctrl.signal.aborted || reqIdRef.current !== thisReq || frameRef.current !== targetFrame) return;
        const msg = err instanceof Error ? err.message : "Frame read failed";
        setFrameError(msg);

        let retrySeconds = 0;
        if (err instanceof ApiError) {
          if (err.retryAfter && err.retryAfter > 0) {
            retrySeconds = err.retryAfter;
          } else if (err.status === 429) {
            retrySeconds = 1;
          }
        }
        if (retrySeconds > 0) {
          const waitMs = Math.max(1000, retrySeconds * 1000);
          setRetryAt(Date.now() + waitMs);
          timerRef.current = setTimeout(() => {
            if (!aborted && frameRef.current === targetFrame) {
              void doFetch();
            }
          }, waitMs);
        }
      }
    };

    void doFetch();

    return () => {
      aborted = true;
      ctrl.abort();
      if (timerRef.current) clearTimeout(timerRef.current);
    };
  }, [frame, totalFrames]);

  const now = Date.now();
  const throttled = retryAt > now;

  const requestHeader = (key: RequestSortKey, label: string) => {
    const active = eventSort.key === key;
    const ariaSort = active ? (eventSort.direction === "asc" ? "ascending" : "descending") : "none";
    return (
      <th aria-sort={ariaSort}>
        <button
          type="button"
          class="flex w-full cursor-pointer items-center gap-1 text-left hover:text-fg"
          onClick={() =>
            setEventSort((current) => ({
              key,
              direction: current.key === key && current.direction === "desc" ? "asc" : "desc",
            }))
          }
        >
          {label}
          <span class="inline-block w-2 text-[8px] leading-none text-accent" aria-hidden="true">
            {active ? (eventSort.direction === "asc" ? "▲" : "▼") : ""}
          </span>
        </button>
      </th>
    );
  };

  return (
    <>
      <PageHeader title="Usage log" sub="Request-level detail, newest first">
        <HealthPill />
        <span class="text-xs text-dim">
          {statsUpdated ? `Updated ${Math.max(0, Math.round((now - statsUpdated) / 1000))}s ago` : "Waiting for data"} ·{" "}
          {pollingPaused ? "auto-refresh paused" : "auto-refresh 2s"}
        </span>
      </PageHeader>

      <Card>
        <CardHead
          title="Request log"
          sub={throttled ? "throttled" : frameData ? `frame ${frameData.frame} of ${totalFrames}` : `${logEvents.length} recent events`}
        >
          <button class="btn btn-sm" onClick={() => setFrame(null)} disabled={frame == null}>
            Latest
          </button>
        </CardHead>
        {frameError && (
          <div class="border-b border-bad bg-bad-soft px-[15px] py-2 text-xs text-bad">
            {frameError}
            {throttled ? ` Retry in ${Math.ceil((retryAt - now) / 1000)}s.` : ""}
          </div>
        )}
        {logEvents.length === 0 ? (
          <Empty title="No requests yet" body="The log fills as soon as traffic arrives." />
        ) : (
          <table class="table">
            <thead>
              <tr>
                {requestHeader("time", "Time")}
                {requestHeader("model", "Model")}
                {requestHeader("streamed", "Stream")}
                {requestHeader("input_tokens", "In")}
                {requestHeader("output_tokens", "Out")}
                {requestHeader("ms", "Latency")}
                {requestHeader("ttfb_ms", "TTFB")}
                {requestHeader("error", "Status")}
              </tr>
            </thead>
            <tbody>
              {sortedEvents.map((e, i) => (
                <tr key={`${e.time}-${i}`}>
                  <td class="muted">{clock(e.time)}</td>
                  <td class="mono">{e.model}</td>
                  <td>{e.streamed ? "yes" : "no"}</td>
                  <td class="tabular-nums">{fmtInt(e.input_tokens)}</td>
                  <td class="tabular-nums">{fmtInt(e.output_tokens)}</td>
                  <td class="tabular-nums">{fmtMs(e.ms)}</td>
                  <td class="tabular-nums">{fmtMs(e.ttfb_ms)}</td>
                  <td>
                    <span class={`pill ${e.error ? "pill-bad" : "pill-ok"}`}>{e.error ? "error" : "ok"}</span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {logEvents.length > 0 && (() => {
          const w = usagePageWindow(frame, totalFrames, logEvents.length);
          return (
            <Pager
              from={w.from}
              to={w.to}
              note={w.note}
              hasPrev={w.hasNewer}
              hasNext={w.hasOlder}
              onPrev={() => setFrame(w.newer)}
              onNext={() => setFrame(w.older)}
            />
          );
        })()}
      </Card>
    </>
  );
}