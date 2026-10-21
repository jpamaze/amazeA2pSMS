"use client";

import { useEffect, useRef, useState } from "react";

// useWidth sizes a chart to its container, so axis text stays at its real
// size instead of scaling with the drawing.
function useWidth(): [React.RefObject<HTMLDivElement | null>, number] {
  const ref = useRef<HTMLDivElement | null>(null);
  const [w, setW] = useState(640);
  useEffect(() => {
    if (!ref.current) return;
    const ro = new ResizeObserver(([e]) => setW(Math.max(280, Math.round(e.contentRect.width))));
    ro.observe(ref.current);
    return () => ro.disconnect();
  }, []);
  return [ref, w];
}

// Minimal SVG charts (no chart library): a line chart for time series and a
// stacked bar chart for counts per period.

type Series = { name: string; color: string; points: [number, number][] }; // [x (ms), y]

function niceMax(v: number) {
  if (v <= 0) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v) return m * p;
  return 10 * p;
}

const fmtY = (v: number) => (v >= 1000 ? `${(v / 1000).toFixed(v >= 10000 ? 0 : 1)}k` : v >= 10 ? v.toFixed(0) : v.toFixed(v === 0 ? 0 : 1));

export function LineChart({ series, height = 180, xLabel }: { series: Series[]; height?: number; xLabel?: (x: number) => string }) {
  const [ref, W] = useWidth();
  const H = height, L = 44, B = 22, T = 8, R = 8;
  const all = series.flatMap((s) => s.points);
  if (all.length < 2) return <div ref={ref} className="empty">Not enough data yet</div>;
  const xs = all.map((p) => p[0]);
  const x0 = Math.min(...xs), x1 = Math.max(...xs);
  const yMax = niceMax(Math.max(...all.map((p) => p[1])));
  const sx = (x: number) => L + ((x - x0) / Math.max(x1 - x0, 1)) * (W - L - R);
  const sy = (y: number) => T + (1 - y / yMax) * (H - T - B);
  const ticks = [0, 0.25, 0.5, 0.75, 1].map((f) => f * yMax);
  const xt = [0, 0.5, 1].map((f) => x0 + f * (x1 - x0));
  const label = xLabel ?? ((x: number) => new Date(x).toLocaleTimeString("en-IN", { timeZone: "Asia/Kolkata", hour: "2-digit", minute: "2-digit" }));
  return (
    <figure className="chart" ref={ref}>
      <svg viewBox={`0 0 ${W} ${H}`} width={W} height={H} role="img" aria-label={series.map((s) => s.name).join(", ")}>
        {ticks.map((t) => (
          <g key={t}>
            <line x1={L} x2={W - R} y1={sy(t)} y2={sy(t)} className="grid" />
            <text x={L - 6} y={sy(t) + 4} textAnchor="end" className="axis">{fmtY(t)}</text>
          </g>
        ))}
        {xt.map((x, i) => (
          <text key={i} x={sx(x)} y={H - 6} textAnchor={i === 0 ? "start" : i === 2 ? "end" : "middle"} className="axis">{label(x)}</text>
        ))}
        {series.map((s) =>
          s.points.length > 1 ? (
            <polyline key={s.name} fill="none" stroke={s.color} strokeWidth={2} points={s.points.map((p) => `${sx(p[0])},${sy(p[1])}`).join(" ")}>
              <title>{s.name}</title>
            </polyline>
          ) : null,
        )}
      </svg>
      <figcaption className="legend">
        {series.map((s) => (
          <span key={s.name}><i style={{ background: s.color }} /> {s.name}</span>
        ))}
      </figcaption>
    </figure>
  );
}

export function BarChart({ bars, keys, height = 180 }: { bars: { label: string; values: Record<string, number> }[]; keys: { key: string; name: string; color: string }[]; height?: number }) {
  const [ref, W] = useWidth();
  const H = height, L = 44, B = 22, T = 8, R = 8;
  if (bars.length === 0) return <div ref={ref} className="empty">No traffic in this period</div>;
  const yMax = niceMax(Math.max(...bars.map((b) => keys.reduce((s, k) => s + (b.values[k.key] ?? 0), 0))));
  const bw = (W - L - R) / bars.length;
  const sy = (y: number) => (y / yMax) * (H - T - B);
  const ticks = [0, 0.5, 1].map((f) => f * yMax);
  return (
    <figure className="chart" ref={ref}>
      <svg viewBox={`0 0 ${W} ${H}`} width={W} height={H} role="img" aria-label="traffic">
        {ticks.map((t) => (
          <g key={t}>
            <line x1={L} x2={W - R} y1={H - B - sy(t)} y2={H - B - sy(t)} className="grid" />
            <text x={L - 6} y={H - B - sy(t) + 4} textAnchor="end" className="axis">{fmtY(t)}</text>
          </g>
        ))}
        {bars.map((b, i) => {
          let acc = 0;
          return (
            <g key={b.label}>
              {keys.map((k) => {
                const v = b.values[k.key] ?? 0;
                const h = sy(v);
                const y = H - B - sy(acc) - h;
                acc += v;
                return (
                  <rect key={k.key} x={L + i * bw + Math.min(bw * 0.15, 6)} width={Math.max(bw - 2 * Math.min(bw * 0.15, 6), 1)} y={y} height={h} fill={k.color}>
                    <title>{`${b.label}: ${k.name} ${v}`}</title>
                  </rect>
                );
              })}
              {(i % Math.ceil(bars.length / 8) === 0) && (
                <text x={L + i * bw + bw / 2} y={H - 6} textAnchor="middle" className="axis">{b.label}</text>
              )}
            </g>
          );
        })}
      </svg>
      <figcaption className="legend">
        {keys.map((k) => (
          <span key={k.key}><i style={{ background: k.color }} /> {k.name}</span>
        ))}
      </figcaption>
    </figure>
  );
}
