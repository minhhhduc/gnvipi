export function buildUpstreamModel(alias: string, upstream: string): string {
  const a = alias.trim();
  const u = upstream.trim();
  return a.endsWith("/*") ? u : (u || a);
}

export const STANDARD_PROTOCOLS = [
  "openai-chat",
  "anthropic-messages",
  "openai-responses",
] as const;

export function getProtocolOptions(currentProtocol?: string, initialProtocol?: string): string[] {
  const custom = new Set<string>();
  if (initialProtocol && !(STANDARD_PROTOCOLS as readonly string[]).includes(initialProtocol)) {
    custom.add(initialProtocol);
  }
  if (currentProtocol && !(STANDARD_PROTOCOLS as readonly string[]).includes(currentProtocol)) {
    custom.add(currentProtocol);
  }
  return [...custom, ...STANDARD_PROTOCOLS];
}

export function computeAutoModeOptions(
  catalogModels: { id: string; hidden?: boolean }[],
  currentTarget?: string,
): string[] {
  const visible = catalogModels.filter((m) => !m.hidden).map((m) => m.id);
  const set = new Set(visible);
  if (currentTarget && currentTarget !== "cycle" && !set.has(currentTarget)) {
    visible.push(currentTarget);
  }
  visible.sort();
  return [...visible, "cycle"];
}

export function computeCycleAvailableOptions(
  catalogModels: { id: string; hidden?: boolean }[],
  cycleModels: string[],
  availableModels: string[],
): string[] {
  const visibleCatalog = catalogModels.filter((m) => !m.hidden).map((m) => m.id);
  const combined = new Set([...cycleModels, ...availableModels, ...visibleCatalog]);
  combined.delete("cycle");
  return [...combined].sort();
}

export function computeCycleUnselected(
  catalogModels: { id: string; hidden?: boolean }[],
  cycleModels: string[],
  availableModels: string[],
): string[] {
  const hiddenSet = new Set(catalogModels.filter((m) => m.hidden).map((m) => m.id));
  const pool = computeCycleAvailableOptions(catalogModels, cycleModels, availableModels);
  const selectedSet = new Set(cycleModels);
  return pool.filter((id) => !selectedSet.has(id) && !hiddenSet.has(id));
}

export function clampFrame(target: number, totalFrames: number): number {
  if (totalFrames <= 0) return 1;
  return Math.max(1, Math.min(totalFrames, target));
}

export function getFrameNavState(frame: number | null, totalFrames: number) {
  const canPrev = frame != null && frame > 1 && totalFrames > 1;
  const canLatest = frame != null;
  const canNext = totalFrames > 0 && (frame == null ? totalFrames >= 1 : frame < totalFrames);
  const nextTarget = frame == null ? 1 : clampFrame(frame + 1, totalFrames);
  const prevTarget = frame == null ? null : clampFrame(frame - 1, totalFrames);
  return { canPrev, canLatest, canNext, prevTarget, nextTarget };
}

export function runningWorkersRatio(chromes?: { killed?: boolean }[]): string {
  const list = chromes || [];
  const running = list.filter((c) => !c.killed).length;
  return `${running} / ${list.length}`;
}

export function formatLastOk(value?: string | null): string {
  if (!value) return "never";
  const d = new Date(value);
  if (isNaN(d.getTime()) || d.getFullYear() <= 1) return "never";
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}
