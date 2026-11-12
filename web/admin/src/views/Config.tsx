import { useEffect, useMemo, useRef, useState } from "preact/hooks";
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

interface SearchSelection {
  engine: string;
  url: string;
}

export function engineForSearchUrl(url: string): string {
  return ENGINES.some(([value]) => value !== "custom" && value === url) ? url : "custom";
}

export function syncSearchUrl(url: string): SearchSelection {
  return { engine: "custom", url };
}

export function syncSearchEngine(engine: string, currentUrl: string): SearchSelection & { save: boolean } {
  return engine === "custom"
    ? { engine, url: currentUrl, save: false }
    : { engine, url: engine, save: true };
}

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
  const confirmedAuto = useRef("");
  const confirmedCycleModels = useRef<string[]>([]);
  const confirmedSearch = useRef<SearchSelection>({ engine: "", url: "" });
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    Promise.all([
      api<ModelsResponse>("/admin/api/models"),
      api<{ model: string }>("/admin/api/config/automode"),
      api<CycleConfig>("/admin/api/config/cycle"),
      api<{ url: string }>("/admin/api/config/search"),
    ])
      .then(([m, a, c, s]) => {
        const model = a.model || "";
        const models = c.models || [];
        const url = s.url || "";
        const search = { engine: engineForSearchUrl(url), url };
        confirmedAuto.current = model;
        confirmedCycleModels.current = models;
        confirmedSearch.current = search;
        setCatalog(m);
        setAuto(model);
        setCycle({ ...c, models, available: c.available || [] });
        setSearchUrl(search.url);
        setEngine(search.engine);
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
      confirmedAuto.current = model;
      setAuto(model);
      setAutoState("saved");
    } catch (err) {
      setAuto(confirmedAuto.current);
      setAutoState("error");
      toast(`Auto model save failed: ${err instanceof Error ? err.message : "unknown"}`, "bad");
    }
  };

  const saveSearch = async (next: SearchSelection) => {
    setSearchState("saving");
    try {
      await api("/admin/api/config/search", { method: "POST", body: { url: next.url } });
      confirmedSearch.current = next;
      setEngine(next.engine);
      setSearchUrl(next.url);
      setSearchState("saved");
      toast("Search setting saved");
    } catch (err) {
      setEngine(confirmedSearch.current.engine);
      setSearchUrl(confirmedSearch.current.url);
      setSearchState("error");
      toast(`Search save failed: ${err instanceof Error ? err.message : "unknown"}`, "bad");
    }
  };

  const chooseSearchEngine = (selectedEngine: string) => {
    const next = syncSearchEngine(selectedEngine, searchUrl);
    setEngine(next.engine);
    setSearchUrl(next.url);
    if (next.save) void saveSearch(next);
  };

  const saveCycle = async (models: string[]) => {
    setCycle((prev) => ({ ...prev, models }));
    setCycleState("saving");
    try {
      await api("/admin/api/config/cycle", { method: "POST", body: { models } });
      confirmedCycleModels.current = models;
      setCycle((prev) => ({ ...prev, models }));
      setCycleState("saved");
    } catch (err) {
      setCycle((prev) => ({ ...prev, models: confirmedCycleModels.current }));
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
              onChange={(e) => chooseSearchEngine((e.target as HTMLSelectElement).value)}
              onClick={(e) => {
                const selectedEngine = (e.currentTarget as HTMLSelectElement).value;
                const next = syncSearchEngine(selectedEngine, searchUrl);
                if (selectedEngine === engine && next.save) void saveSearch(next);
              }}
            >
              {ENGINES.map(([value, label]) => <option key={label} value={value}>{label}</option>)}
            </select>
            <label class="mb-1 mt-2.5 block text-xs font-semibold">Search URL</label>
            <input
              class="input mono"
              value={searchUrl}
              onInput={(e) => {
                const next = syncSearchUrl((e.target as HTMLInputElement).value);
                setEngine(next.engine);
                setSearchUrl(next.url);
              }}
            />
            <div class="mt-2 flex justify-end"><button class="btn btn-sm" onClick={() => void saveSearch({ engine: "custom", url: searchUrl })}>Save URL</button></div>
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
