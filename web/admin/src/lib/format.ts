export function fmtInt(n: number | undefined | null): string {
  if (n == null || !Number.isFinite(n)) return "—";
  return Math.round(n).toLocaleString();
}

export function fmtCompact(n: number | undefined | null): string {
  if (n == null || !Number.isFinite(n)) return "—";
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(2).replace(/\.?0+$/, "") + "M";
  if (n >= 1_000) return (n / 1_000).toFixed(1).replace(/\.0$/, "") + "K";
  return String(Math.round(n));
}

export function fmtMs(n: number | undefined | null): string {
  if (n == null || !Number.isFinite(n) || n <= 0) return "—";
  return Math.round(n) + "ms";
}

export function fmtPercent(errors: number, requests: number): string {
  if (!requests) return "0%";
  return ((errors / requests) * 100).toFixed(1) + "%";
}

export function p95(values: number[]): number | null {
  const clean = values.filter((v) => Number.isFinite(v) && v >= 0).sort((a, b) => a - b);
  if (!clean.length) return null;
  const idx = Math.min(clean.length - 1, Math.floor(clean.length * 0.95));
  return clean[idx];
}

export function avg(values: number[]): number | null {
  const clean = values.filter((v) => Number.isFinite(v) && v > 0);
  if (!clean.length) return null;
  return clean.reduce((a, b) => a + b, 0) / clean.length;
}

export function parseTime(value?: string): number | null {
  if (!value) return null;
  const t = Date.parse(value);
  return Number.isFinite(t) ? t : null;
}

export function uptime(startedAt?: string): string {
  const t = parseTime(startedAt);
  if (t == null) return "—";
  const secs = Math.max(0, (Date.now() - t) / 1000);
  const h = Math.floor(secs / 3600);
  const m = Math.floor((secs % 3600) / 60);
  if (h >= 24) return `${Math.floor(h / 24)}d ${h % 24}h`;
  return `${h}h ${m}m`;
}

export function clock(value?: string): string {
  const t = parseTime(value);
  if (t == null) return "—";
  return new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

export function minuteLabel(minute: string): string {
  // Backend minutes are ISO-ish; fall back to the raw value.
  const t = Date.parse(minute);
  if (!Number.isFinite(t)) return minute;
  return new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

export function secondsToNs(sec: string): number {
  const n = parseFloat(sec);
  return Number.isFinite(n) && n > 0 ? Math.round(n * 1e9) : 0;
}

export function nsToSeconds(ns?: number): string {
  return ns && ns > 0 ? String(ns / 1e9) : "";
}
