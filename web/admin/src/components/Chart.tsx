import { useLayoutEffect, useRef, useState } from "preact/hooks";
import { niceTicks, segments, smoothPath } from "../lib/chart";

export interface ChartSeries {
  label: string;
  color: string;
  values: (number | null)[];
}

interface Props {
  series: ChartSeries[];
  labels: string[];
  height?: number;
  emptyMessage?: string;
  /** Formats a value for the hover tooltip and the last-point chip. */
  format?: (value: number) => string;
}

const DEFAULT_WIDTH = 560;
// Room for a four-digit tick label at 10px tabular figures.
const PL = 44;
const PR = 12;
const PT = 12;
const PB = 24;

export function LineChart({ series, labels, height = 176, emptyMessage, format }: Props) {
  const svgRef = useRef<SVGSVGElement>(null);
  const [width, setWidth] = useState(DEFAULT_WIDTH);
  const [hover, setHover] = useState<number | null>(null);
  const pointCount = labels.length;
  const all = series.flatMap((s) => s.values).filter((v): v is number => v != null);
  const dataMax = Math.max(0, ...all);
  const ticks = niceTicks(dataMax);
  const yMax = ticks[ticks.length - 1] || 1;
  const hasData = all.length > 0 && dataMax !== 0;
  const fmt = format ?? ((value: number) => String(Math.round(value * 10) / 10));

  useLayoutEffect(() => {
    if (!hasData) return;
    const svg = svgRef.current;
    if (!svg) return;

    const initialWidth = Math.round(svg.getBoundingClientRect().width);
    if (initialWidth > 0) setWidth(initialWidth);
    const observer = new ResizeObserver(([entry]) => {
      const nextWidth = Math.round(entry.contentRect.width);
      if (nextWidth > 0) setWidth(nextWidth);
    });
    observer.observe(svg);
    return () => observer.disconnect();
  }, [hasData]);

  const iw = width - PL - PR;
  const ih = height - PT - PB;
  const x = (i: number) => PL + (pointCount <= 1 ? 0 : (i / (pointCount - 1)) * iw);
  const y = (v: number) => PT + ih - (v / yMax) * ih;

  if (!hasData) {
    return (
      <div class="flex h-[176px] flex-col items-center justify-center gap-1 text-center text-dim">
        <span class="text-sm font-semibold text-fg">No requests yet</span>
        <span class="text-xs">{emptyMessage || "Charts appear after the first upstream call."}</span>
      </div>
    );
  }

  const labelStep = Math.max(1, Math.ceil(pointCount / 5));
  const hoverPoint = hover === null ? null : { index: hover, x: x(hover) };
  const tooltipLeft = hoverPoint ? Math.min(Math.max(hoverPoint.x + 10, PL), width - 132) : 0;

  const pick = (event: MouseEvent) => {
    const rect = (event.currentTarget as HTMLElement).getBoundingClientRect();
    const px = event.clientX - rect.left;
    if (pointCount <= 1) {
      setHover(0);
      return;
    }
    const index = Math.round(((px - PL) / iw) * (pointCount - 1));
    setHover(Math.min(pointCount - 1, Math.max(0, index)));
  };

  return (
    <div class="w-full">
      {series.length > 1 && (
        <div class="mb-2 flex flex-wrap gap-x-4 gap-y-1 text-[11px] text-dim">
          {series.map((entry) => (
            <span key={entry.label} class="flex items-center gap-1.5">
              <span class="inline-block h-[2px] w-4 rounded-full" style={{ background: entry.color }} />
              {entry.label}
            </span>
          ))}
        </div>
      )}
      <div class="relative w-full" onMouseMove={pick} onMouseLeave={() => setHover(null)}>
        <svg ref={svgRef} width="100%" height={height} class="block" role="img" aria-label={series.map((s) => s.label).join(", ")}>
          {ticks.map((t) => (
            <g key={t}>
              <line
                x1={PL}
                x2={width - PR}
                y1={y(t)}
                y2={y(t)}
                stroke={t === 0 ? "var(--line)" : "var(--line)"}
                stroke-width="1"
                stroke-dasharray={t === 0 ? undefined : "2 4"}
                opacity={t === 0 ? 1 : 0.55}
              />
              <text x={PL - 8} y={y(t) + 3.5} text-anchor="end" font-size="10" fill="var(--dim)" style={{ fontVariantNumeric: "tabular-nums" }}>
                {t}
              </text>
            </g>
          ))}
          {labels.map((label, i) =>
            i % labelStep === 0 || i === pointCount - 1 ? (
              <text
                key={`${label}-${i}`}
                x={i === pointCount - 1 ? width - PR : x(i)}
                y={height - 6}
                text-anchor={i === pointCount - 1 ? "end" : "middle"}
                font-size="10"
                fill="var(--dim)"
              >
                {label}
              </text>
            ) : null,
          )}

          {series.map((s) => {
            const parts = segments(s.values);
            const showArea = series.length === 1;
            return (
              <g key={s.label}>
                {parts.map((part, pi) => {
                  if (part.length === 1) {
                    return <circle key={pi} cx={x(part[0].index)} cy={y(part[0].value)} r="2.5" fill={s.color} />;
                  }
                  const line = smoothPath(part.map((p) => ({ x: x(p.index), y: y(p.value) })));
                  return (
                    <g key={pi}>
                      {showArea && (
                        <path
                          d={`${line} L${x(part[part.length - 1].index).toFixed(1)},${y(0)} L${x(part[0].index).toFixed(1)},${y(0)} Z`}
                          fill={s.color}
                          opacity="0.08"
                        />
                      )}
                      <path d={line} fill="none" stroke={s.color} stroke-width="2" stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke" />
                    </g>
                  );
                })}
              </g>
            );
          })}

          {hoverPoint && (
            <g>
              <line x1={hoverPoint.x} x2={hoverPoint.x} y1={PT} y2={PT + ih} stroke="var(--dim2)" stroke-width="1" stroke-dasharray="2 3" />
              {series.map((s) => {
                const value = s.values[hoverPoint.index];
                if (value == null) return null;
                return <circle key={s.label} cx={hoverPoint.x} cy={y(value)} r="3" fill={s.color} stroke="var(--panel)" stroke-width="1.5" />;
              })}
            </g>
          )}
        </svg>

        {hoverPoint && (
          <div
            class="pointer-events-none absolute top-1 z-10 min-w-[120px] rounded-[6px] border border-line bg-panel px-2 py-1.5 text-[11px] shadow-sm"
            style={{ left: `${tooltipLeft}px` }}
          >
            <div class="mb-0.5 text-dim">{labels[hoverPoint.index]}</div>
            {series.map((s) => {
              const value = s.values[hoverPoint.index];
              return (
                <div key={s.label} class="flex items-center justify-between gap-3">
                  <span class="flex items-center gap-1.5 text-dim">
                    <span class="inline-block size-1.5 rounded-full" style={{ background: s.color }} />
                    {s.label}
                  </span>
                  <span class="tabular-nums">{value == null ? "—" : fmt(value)}</span>
                </div>
              );
            })}
          </div>
        )}
      </div>
    </div>
  );
}
