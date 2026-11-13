import { useLayoutEffect, useRef, useState } from "preact/hooks";
import { niceTicks, segments } from "../lib/chart";

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
}

const DEFAULT_WIDTH = 560;
const PL = 38;
const PR = 10;
const PT = 8;
const PB = 20;

export function LineChart({ series, labels, height = 168, emptyMessage }: Props) {
  const svgRef = useRef<SVGSVGElement>(null);
  const [width, setWidth] = useState(DEFAULT_WIDTH);
  const pointCount = labels.length;
  const all = series.flatMap((s) => s.values).filter((v): v is number => v != null);
  const dataMax = Math.max(0, ...all);
  const ticks = niceTicks(dataMax);
  const yMax = ticks[ticks.length - 1] || 1;
  const hasData = all.length > 0 && dataMax !== 0;

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
      <div class="flex h-[168px] flex-col items-center justify-center gap-1 text-center text-dim">
        <span class="text-sm font-semibold text-fg">No requests yet</span>
        <span class="text-xs">{emptyMessage || "Charts appear after the first upstream call."}</span>
      </div>
    );
  }

  const labelStep = Math.max(1, Math.ceil(pointCount / 5));
  return (
    <div class="w-full">
      {series.length > 1 && (
        <div class="mb-1 flex flex-wrap gap-x-4 gap-y-1 text-[11px] text-dim">
          {series.map((s) => (
            <span key={s.label}>
              <span class="mr-1.5 inline-block size-2 rounded-full" style={{ background: s.color }} />
              {s.label}
            </span>
          ))}
        </div>
      )}
      <svg ref={svgRef} width="100%" height={height} class="block">
        {ticks.map((t) => (
          <g key={t}>
            <line x1={PL} x2={width - PR} y1={y(t)} y2={y(t)} stroke="var(--line)" stroke-width="1" />
            <text x={PL - 6} y={y(t) + 3.5} text-anchor="end" font-size="9" fill="var(--dim)">
              {t}
            </text>
          </g>
        ))}
        {labels.map((label, i) =>
          i % labelStep === 0 || i === pointCount - 1 ? (
            <text
              key={`${label}-${i}`}
              x={i === pointCount - 1 ? width - PR : x(i)}
              y={height - 4}
              text-anchor={i === pointCount - 1 ? "end" : "middle"}
              font-size="9"
              fill="var(--dim)"
            >
              {label}
            </text>
          ) : null,
        )}
        {series.map((s) => {
          const parts = segments(s.values);
          return (
            <g key={s.label}>
              {parts.map((part, pi) => {
                if (part.length === 1) {
                  return <circle key={pi} cx={x(part[0].index)} cy={y(part[0].value)} r="2" fill={s.color} />;
                }
                const d = part
                  .map((p, i) => `${i === 0 ? "M" : "L"}${x(p.index).toFixed(1)},${y(p.value).toFixed(1)}`)
                  .join(" ");
                return (
                  <path
                    key={pi}
                    d={d}
                    fill="none"
                    stroke={s.color}
                    stroke-width="1.8"
                    vector-effect="non-scaling-stroke"
                  />
                );
              })}
            </g>
          );
        })}
      </svg>
    </div>
  );
}
