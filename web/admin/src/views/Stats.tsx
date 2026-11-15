import type { ComponentChildren } from "preact";
import { useEffect, useMemo, useState } from "preact/hooks";
import { useGateway } from "../lib/context";
import { avg, fmtCompact, fmtInt, fmtMs, fmtPercent, minuteLabel, p95, uptime } from "../lib/format";
import { modelSetKey, seriesValues, topModelNames, ttfbValues } from "../lib/chart";
import type { RequestEvent, Stats as StatsData } from "../lib/types";
import { Card, CardHead, PageHeader } from "../components/ui";
import { HealthPill } from "../components/Shell";
import { LineChart } from "../components/Chart";

const NO_EVENTS: RequestEvent[] = [];

const COLORS = {
  req: "var(--accent)",
  input: "var(--on)",
  output: "var(--warn)",
  ttfb: "var(--dim2)",
  top: ["var(--accent)", "var(--on)", "var(--warn)", "var(--dim2)", "var(--dim)"],
};

function Kpi({ label, value, detail }: { label: string; value: string; detail: string }) {
  return (
    <div class="bg-panel px-3.5 py-3">
      <div class="text-[11px] text-dim">{label}</div>
      <div class="mt-1 text-[22px] font-semibold tabular-nums tracking-[-0.02em]">{value}</div>
      <div class="mt-0.5 text-[11px] text-dim">{detail}</div>
    </div>
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
  const [pollingPaused, setPollingPaused] = useState(() => document.hidden);
  const data: StatsData = stats || {};
  const series = useMemo(
    () => (data.series || []).slice().sort((a, b) => a.minute.localeCompare(b.minute)),
    [data.series],
  );
  // KPIs read the live ring only; an older frame belongs to the Usage log.
  const kpiEvents = data.events ?? NO_EVENTS;

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
  useEffect(() => {
    const onVisibilityChange = () => setPollingPaused(document.hidden);
    document.addEventListener("visibilitychange", onVisibilityChange);
    return () => document.removeEventListener("visibilitychange", onVisibilityChange);
  }, []);

  const now = Date.now();

  return (
    <>
      <PageHeader title="Dashboard" sub="Live traffic across every upstream API">
        <HealthPill />
        <span class="text-xs text-dim">
          {statsUpdated ? `Updated ${Math.max(0, Math.round((now - statsUpdated) / 1000))}s ago` : "Waiting for data"} ·{" "}
          {pollingPaused ? "auto-refresh paused" : "auto-refresh 2s"}
        </span>
      </PageHeader>

      <Card class="mb-3 px-[15px] py-3 text-xs text-dim">
        <div class="mb-1 font-semibold text-fg">Reading the numbers</div>
        <div class="space-y-0.5">
          <div>Totals, tokens and errors are cumulative <b class="text-fg">since start</b>; TTFB and p95 read the <b class="text-fg">last 200 events</b>.</div>
          <div>A gap in the TTFB chart means no streamed request in that minute — it is not zero latency.</div>
          <div>Panels refresh every 2s while the tab is visible; per-request detail lives on the Usage log.</div>
          <div>Requests are attributed per gateway account; these totals cover the whole gateway.</div>
        </div>
      </Card>

      <Card class="mb-3 overflow-hidden">
      <div class="grid grid-cols-2 gap-px bg-[var(--line)] sm:grid-cols-4">
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
      </Card>

      <div class="mb-3 grid grid-cols-2 gap-3">
        <ChartCard title="Requests per minute" sub="last 60 min">
          <LineChart series={[{ label: "requests / min", color: COLORS.req, values: seriesValues(series) }]} labels={labels} format={fmtInt} />
        </ChartCard>
        <ChartCard title="Tokens per minute" sub="input vs output">
          <LineChart
            series={[
              { label: "input tokens / min", color: COLORS.input, values: series.map((m) => m.input_tokens) },
              { label: "output tokens / min", color: COLORS.output, values: series.map((m) => m.output_tokens) },
            ]}
            labels={labels}
            format={fmtInt}
          />
        </ChartCard>
        <ChartCard title="Average TTFB" sub="streamed requests · gaps = no streamed request">
          <LineChart
            series={[{ label: "avg TTFB / min", color: COLORS.ttfb, values: ttfbValues(series) }]}
            labels={labels}
            format={fmtMs}
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
            format={fmtInt}
          />
        </ChartCard>
      </div>
    </>
  );
}
