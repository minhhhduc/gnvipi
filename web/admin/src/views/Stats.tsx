import type { ComponentChildren } from "preact";
import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { api, ApiError } from "../lib/api";
import { useGateway } from "../lib/context";
import { avg, clock, fmtCompact, fmtInt, fmtMs, fmtPercent, minuteLabel, p95, uptime } from "../lib/format";
import { clampFrame, getFrameNavState } from "../lib/helpers";
import { modelSetKey, seriesValues, sortRequestEvents, topModelNames, ttfbValues } from "../lib/chart";
import type { RequestSortKey, SortDirection } from "../lib/chart";
import type { FrameResponse, RequestEvent, Stats as StatsData } from "../lib/types";
import { Card, CardHead, Empty, PageHeader } from "../components/ui";
import { HealthPill } from "../components/Shell";
import { LineChart } from "../components/Chart";

const NO_EVENTS: RequestEvent[] = [];

// The KPI tiles and latency charts label themselves "last 200 events", which is
// the live ring. Once the operator opens an older frame the request log must
// follow that frame, but the KPIs must not: a stale frame would silently
// rewrite totals that only ever describe now.
export function selectStatsEvents(live?: RequestEvent[], frame?: RequestEvent[]) {
  const kpiEvents = live ?? NO_EVENTS;
  return { kpiEvents, logEvents: frame ?? kpiEvents };
}

const COLORS = {
  req: "var(--accent)",
  input: "var(--on)",
  output: "var(--warn)",
  ttfb: "var(--dim2)",
  top: ["var(--accent)", "var(--on)", "var(--warn)", "#a855f7", "#06b6d4"],
};

function Kpi({ label, value, detail }: { label: string; value: string; detail: string }) {
  return (
    <Card class="px-[13px] py-3">
      <div class="text-[11px] font-semibold uppercase tracking-[0.05em] text-dim">{label}</div>
      <div class="mt-0.5 text-[25px] font-bold tabular-nums tracking-[-0.03em]">{value}</div>
      <div class="mt-0.5 text-[11px] text-dim">{detail}</div>
    </Card>
  );
}

function ChartCard({ title, sub, children }: { title: string; sub: string; children: ComponentChildren }) {
  return (
    <Card>
      <CardHead title={title} sub={sub} />
      <div class="px-[15px] py-3.5">{children}</div>
    </Card>
  );
}

export function StatsView() {
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
  const series = useMemo(
    () => (data.series || []).slice().sort((a, b) => a.minute.localeCompare(b.minute)),
    [data.series],
  );
  const { kpiEvents, logEvents } = selectStatsEvents(data.events, frameData?.events);
  const totalFrames = frameData?.total_frames ?? data.total_frames ?? 0;

  const models = data.models || [];
  const totalRequests = models.reduce((n, m) => n + m.requests, 0);
  const totalErrors = models.reduce((n, m) => n + m.errors, 0);
  const inputTokens = models.reduce((n, m) => n + m.input_tokens, 0);
  const outputTokens = models.reduce((n, m) => n + m.output_tokens, 0);
  const latest = series[series.length - 1];
  const streamed = kpiEvents.filter((e) => e.streamed && e.ttfb_ms > 0);
  const avgTtfb = avg(streamed.map((e) => e.ttfb_ms));
  const p95Ms = p95(kpiEvents.map((e) => e.ms));
  const hasTraffic = totalRequests > 0 || series.some((m) => m.requests > 0);
  // Keep all 8 slots; show "—" until the first successful stats read.
  const unavailable = !stats;
  const value = (v: string) => (unavailable ? "—" : v);
  const labels = series.map((m) => minuteLabel(m.minute));

  const seriesByModel = data.series_by_model || {};
  const topModelKey = modelSetKey(seriesByModel);
  const topModels = useMemo(() => topModelNames(seriesByModel), [topModelKey]);
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
      <PageHeader title="Stats" sub="Live traffic across every upstream API">
        <HealthPill />
        <span class="text-xs text-dim">
          {statsUpdated ? `Updated ${Math.max(0, Math.round((now - statsUpdated) / 1000))}s ago` : "Waiting for data"} ·{" "}
          {pollingPaused ? "auto-refresh paused" : "auto-refresh 2s"}
        </span>
      </PageHeader>

      <div class="mb-3 grid grid-cols-4 gap-2.5">
        <Kpi label="Total requests" value={value(fmtInt(totalRequests))} detail={hasTraffic ? "since start" : "no traffic"} />
        <Kpi label="Req / min" value={value(hasTraffic ? fmtInt(latest?.requests || 0) : "0")} detail={hasTraffic ? "latest minute" : "no traffic"} />
        <Kpi label="Input tokens" value={value(fmtCompact(inputTokens))} detail="input tokens (sum)" />
        <Kpi label="Output tokens" value={value(fmtCompact(outputTokens))} detail="output tokens (sum)" />
        <Kpi
          label="Errors"
          value={value(hasTraffic ? fmtPercent(totalErrors, totalRequests) : "0")}
          detail={hasTraffic ? `${fmtInt(totalErrors)} total` : "no traffic"}
        />
        <Kpi label="Avg TTFB" value={value(fmtMs(avgTtfb))} detail="last 200 events · streamed only" />
        <Kpi label="p95 latency" value={value(fmtMs(p95Ms))} detail="last 200 events" />
        <Kpi label="Uptime" value={data.started_at ? uptime(data.started_at) : "—"} detail="since process start" />
      </div>

      <div class="mb-3 grid grid-cols-2 gap-3">
        <ChartCard title="Requests per minute" sub="last 60 min">
          <LineChart series={[{ label: "requests / min", color: COLORS.req, values: seriesValues(series) }]} labels={labels} />
        </ChartCard>
        <ChartCard title="Tokens per minute" sub="input vs output">
          <LineChart
            series={[
              { label: "input tokens / min", color: COLORS.input, values: series.map((m) => m.input_tokens) },
              { label: "output tokens / min", color: COLORS.output, values: series.map((m) => m.output_tokens) },
            ]}
            labels={labels}
          />
        </ChartCard>
        <ChartCard title="Average TTFB" sub="streamed requests · gaps = no streamed request">
          <LineChart
            series={[{ label: "avg TTFB / min", color: COLORS.ttfb, values: ttfbValues(series) }]}
            labels={labels}
            emptyMessage="No streamed requests yet."
          />
        </ChartCard>
        <ChartCard title="Top models" sub="requests / min · top 5 by window total">
          <LineChart
            series={topModels.map((model, i) => ({
              label: model,
              color: COLORS.top[i % COLORS.top.length],
              values: seriesValues(seriesByModel[model] || []),
            }))}
            labels={labels}
          />
        </ChartCard>
      </div>

      <Card>
        <CardHead
          title="Request log"
          sub={throttled ? "throttled" : frameData ? `frame ${frameData.frame} of ${totalFrames}` : `${logEvents.length} recent events`}
        >
          <div class="flex items-center gap-1">
            {(() => {
              const nav = getFrameNavState(frame, totalFrames);
              return (
                <>
                  <button
                    class="btn btn-sm"
                    disabled={!nav.canPrev}
                    onClick={() => nav.prevTarget != null && setFrame(nav.prevTarget)}
                    aria-label="Previous frame"
                  >
                    «
                  </button>
                  <button class="btn btn-sm" onClick={() => setFrame(null)} disabled={!nav.canLatest}>
                    Latest
                  </button>
                  <button
                    class="btn btn-sm"
                    disabled={!nav.canNext}
                    onClick={() => setFrame(nav.nextTarget)}
                    aria-label="Next frame"
                  >
                    »
                  </button>
                </>
              );
            })()}
          </div>
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
      </Card>
    </>
  );
}
