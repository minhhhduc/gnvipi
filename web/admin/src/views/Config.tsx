import { useEffect, useMemo, useState } from "preact/hooks";
import { api } from "../lib/api";
import { useUi } from "../components/ui";
import { computeAutoModeOptions, computeCycleAvailableOptions, computeCycleUnselected } from "../lib/helpers";
import type { CycleConfig, ModelsResponse } from "../lib/types";
import { Card, CardHead, Empty, IconChevronDown, IconChevronUp, IconX, PageHeader } from "../components/ui";
import { HealthPill } from "../components/Shell";

const ENGINES: [string, string][] = [
  ["", "Disabled"],
  ["https://www.google.com/search?q={query}", "Google"],
  ["https://scholar.google.com/scholar?q={query}", "Google Scholar"],
  ["https://www.bing.com/search?q={query}", "Bing"],
  ["https://www.duckduckgo.com/html/?q={query}", "DuckDuckGo"],
  ["custom", "Custom URL"],
];

type SaveState = "idle" | "saving" | "saved" | "error";

function Saved({ state }: { state: SaveState }) {
  if (state === "saving") return <span class="pill pill-mute">Saving…</span>;
  if (state === "saved") return <span class="pill pill-ok">Saved</span>;
  if (state === "error") return <span class="pill pill-bad">Save failed</span>;
  return <span class="pill pill-mute">Idle</span>;
}

export function ConfigView() {
  const { toast, setBanner } = useUi();
  const [catalog, setCatalog] = useState<ModelsResponse>({});
  const [auto, setAuto] = useState<string>("");
  const [autoState, setAutoState] = useState<SaveState>("idle");
  const [cycle, setCycle] = useState<CycleConfig>({ models: [], available: [], current: "" });
  const [cycleState, setCycleState] = useState<SaveState>("idle");
  const [engine, setEngine] = useState("");
  const [searchUrl, setSearchUrl] = useState("");
  const [searchState, setSearchState] = useState<SaveState>("idle");
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    Promise.all([
      api<ModelsResponse>("/admin/api/models"),
      api<{ model: string }>("/admin/api/config/automode"),
      api<CycleConfig>("/admin/api/config/cycle"),
      api<{ url: string }>("/admin/api/config/search"),
    ])
      .then(([m, a, c, s]) => {
        setCatalog(m);
        setAuto(a.model || "");
        setCycle({ ...c, models: c.models || [], available: c.available || [] });
        setSearchUrl(s.url || "");
        setEngine(ENGINES.some(([v]) => v === (s.url || "")) ? s.url || "" : s.url ? "custom" : "");
        setLoading(false);
      })
      .catch((err) => {
        setLoading(false);
        setBanner(`Config failed to load: ${err instanceof Error ? err.message : "unknown error"}`);
      });
  }, []);

  const saveAuto = async (model: string) => {
    setAuto(model);
    setAutoState("saving");
    try {
      await api("/admin/api/config/automode", { method: "POST", body: { model } });
      setAutoState("saved");
    } catch (err) {
      setAutoState("error");
      toast(`Auto model save failed: ${err instanceof Error ? err.message : "unknown"}`, "bad");
    }
  };

  const saveSearch = async (url: string) => {
    setSearchState("saving");
    try {
      await api("/admin/api/config/search", { method: "POST", body: { url } });
      setSearchState("saved");
      toast("Search setting saved");
    } catch (err) {
      setSearchState("error");
      toast(`Search save failed: ${err instanceof Error ? err.message : "unknown"}`, "bad");
    }
  };

  const saveCycle = async (models: string[]) => {
    setCycle((prev) => ({ ...prev, models }));
    setCycleState("saving");
    try {
      await api("/admin/api/config/cycle", { method: "POST", body: { models } });
      setCycleState("saved");
    } catch (err) {
      setCycleState("error");
      toast(`Cycle save failed: ${err instanceof Error ? err.message : "unknown"}`, "bad");
    }
  };

  const move = (i: number, dir: -1 | 1) => {
    const models = [...(cycle.models || [])];
    const j = i + dir;
    if (j < 0 || j >= models.length) return;
    [models[i], models[j]] = [models[j], models[i]];
    void saveCycle(models);
  };

  const autoOptions = useMemo(
    () => computeAutoModeOptions(catalog.models || [], auto),
    [catalog.models, auto],
  );
  const cyclePool = useMemo(
    () => computeCycleAvailableOptions(catalog.models || [], cycle.models || [], cycle.available || []),
    [catalog.models, cycle.models, cycle.available],
  );
  const unselected = useMemo(
    () => computeCycleUnselected(catalog.models || [], cycle.models || [], cycle.available || []),
    [catalog.models, cycle.models, cycle.available],
  );

  if (loading) return <><PageHeader title="Config" sub="Auto model, web search and the cycle chain"><HealthPill /></PageHeader><Card><Empty title="Loading…" body="Reading configuration." /></Card></>;

  return (
    <>
      <PageHeader title="Config" sub="Auto model, web search and the cycle chain">
        <HealthPill />
      </PageHeader>

      <div class="mb-3 grid grid-cols-2 gap-3">
        <Card>
          <CardHead title="Auto model"><Saved state={autoState} /></CardHead>
          <div class="px-[15px] py-3.5">
            <p class="mb-2.5 text-xs text-dim">Requests with <span class="mono">model: "automode"</span> forward unchanged to this model.</p>
            <label class="mb-1 block text-xs font-semibold">Target model</label>
            <select class="input" value={auto} onChange={(e) => void saveAuto((e.target as HTMLSelectElement).value)}>
              {autoOptions.map((id) => <option key={id} value={id}>{id}</option>)}
            </select>
            <p class="mt-1 text-[11px] text-dim">Current target: <span class="mono">{auto || "—"}</span></p>
          </div>
        </Card>

        <Card>
          <CardHead title="Web search"><Saved state={searchState} /></CardHead>
          <div class="px-[15px] py-3.5">
            <p class="mb-2.5 text-xs text-dim">Backs the gateway search tool. Saves immediately and survives restarts.</p>
            <label class="mb-1 block text-xs font-semibold">Search engine</label>
            <select
              class="input"
              value={engine}
              onChange={(e) => {
                const v = (e.target as HTMLSelectElement).value;
                setEngine(v);
                if (v !== "custom") {
                  setSearchUrl(v);
                  void saveSearch(v);
                }
              }}
            >
              {ENGINES.map(([value, label]) => <option key={label} value={value}>{label}</option>)}
            </select>
            <label class="mb-1 mt-2.5 block text-xs font-semibold">Search URL</label>
            <input class="input mono" value={searchUrl} onInput={(e) => setSearchUrl((e.target as HTMLInputElement).value)} />
            <div class="mt-2 flex justify-end"><button class="btn btn-sm" onClick={() => void saveSearch(searchUrl)}>Save URL</button></div>
          </div>
        </Card>
      </div>

      <Card>
        <CardHead title="Model cycle" sub={`${(cycle.models || []).length} of ${cyclePool.length} selected · selected list always visible`}>
          <Saved state={cycleState} />
        </CardHead>
        <div class="grid grid-cols-2 gap-4 px-[15px] py-3.5">
          <div>
            <h3 class="mb-2 text-xs font-semibold uppercase tracking-[0.05em] text-dim">Selected (priority order)</h3>
            {(cycle.models || []).map((model, i) => (
              <div key={model} class="mb-1.5 flex items-center gap-2 rounded-lg border border-line px-2.5 py-2">
                <span class="chip">{i + 1}</span>
                <span class="mono text-xs">{model}</span>
                <span class="ml-auto flex items-center gap-1.5">
                  <button class="btn btn-sm" aria-label={`Move ${model} up`} disabled={i === 0} onClick={() => move(i, -1)}><IconChevronUp /></button>
                  <button class="btn btn-sm" aria-label={`Move ${model} down`} disabled={i === (cycle.models || []).length - 1} onClick={() => move(i, 1)}><IconChevronDown /></button>
                  <button class="btn btn-sm" aria-label={`Remove ${model}`} onClick={() => void saveCycle((cycle.models || []).filter((m) => m !== model))}><IconX /></button>
                </span>
              </div>
            ))}
            <p class="mt-1 text-[11px] text-dim">Current target: <span class="mono">{cycle.current || "—"}</span></p>
          </div>
          <div>
            <h3 class="mb-2 text-xs font-semibold uppercase tracking-[0.05em] text-dim">Unselected</h3>
            {unselected.map((model) => (
              <div key={model} class="mb-1.5 flex items-center gap-2 rounded-lg border border-dashed border-line2 px-2.5 py-2">
                <span class="mono text-xs">{model}</span>
                <button class="btn btn-sm ml-auto" onClick={() => void saveCycle([...(cycle.models || []), model])}>Add</button>
              </div>
            ))}
          </div>
        </div>
        <div class="flex items-center gap-2 rounded-b-xl border-t border-line bg-panel2 px-[15px] py-2.5">
          <span class="size-[7px] rounded-full bg-on" />
          <span class="text-xs text-dim">{cycleState === "saving" ? "Saving…" : "Changes save automatically"}</span>
        </div>
      </Card>
    </>
  );
}
