import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { api, modelPath } from "../lib/api";
import { useUi } from "../components/ui";
import type { ModelsResponse, Provider, TestResult } from "../lib/types";
import { Card, CardHead, Dialog, Empty, IconDots, PageHeader, Switch } from "../components/ui";
import { HealthPill } from "../components/Shell";
import { formatProviderTestResult, ProviderEditor, ProviderTestResultBadge } from "../components/ProviderEditor";

export function ModelsView() {
  const { toast, setBanner } = useUi();
  const [providers, setProviders] = useState<Provider[]>([]);
  const [catalog, setCatalog] = useState<ModelsResponse>({ models: [], deleted: [] });
  const [loading, setLoading] = useState(true);
  const [query, setQuery] = useState("");
  const [filter, setFilter] = useState<"all" | "on" | "off">("all");
  const [menu, setMenu] = useState<string | null>(null);
  const [editor, setEditor] = useState<{ open: boolean; provider: Provider | null } | null>(null);
  const [confirmDelete, setConfirmDelete] = useState<Provider | null>(null);
  const [confirmModel, setConfirmModel] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [deletedOpen, setDeletedOpen] = useState(false);
  const [tests, setTests] = useState<Record<string, TestResult>>({});
  const [localHidden, setLocalHidden] = useState<Record<string, boolean>>({});
  const localHiddenRef = useRef<Record<string, boolean>>({});
  const pendingTargetRef = useRef<{ hiddenIds: string[]; localState: Record<string, boolean> } | null>(null);
  const activeSaveRef = useRef<boolean>(false);
  const confirmedStateRef = useRef<Record<string, boolean>>({});

  const load = async () => {
    try {
      const [p, m] = await Promise.all([api<Provider[]>("/admin/api/providers"), api<ModelsResponse>("/admin/api/models")]);
      setProviders(p);
      setCatalog(m);
      localHiddenRef.current = {};
      setLocalHidden({});
      confirmedStateRef.current = {};
      pendingTargetRef.current = null;
      setLoading(false);
    } catch (err) {
      setLoading(false);
      setBanner(`Models failed to load: ${err instanceof Error ? err.message : "unknown error"}`, () => void load());
    }
  };

  useEffect(() => {
    void load();
  }, []);

  const models = useMemo(
    () => (catalog.models || []).slice().sort((a, b) => `${a.publisher} ${a.name}`.localeCompare(`${b.publisher} ${b.name}`)),
    [catalog.models],
  );
  const onCount = models.filter((m) => !(localHidden[m.id] ?? m.hidden)).length;

  const drainMaskQueue = async () => {
    if (activeSaveRef.current) return;
    activeSaveRef.current = true;
    while (pendingTargetRef.current) {
      const target = pendingTargetRef.current;
      pendingTargetRef.current = null;
      try {
        await api("/admin/api/models", { method: "POST", body: { masks_id: target.hiddenIds } });
        confirmedStateRef.current = target.localState;
      } catch (err) {
        if (!pendingTargetRef.current) {
          localHiddenRef.current = confirmedStateRef.current;
          setLocalHidden(confirmedStateRef.current);
          await load();
        }
        toast(`Catalog save failed: ${err instanceof Error ? err.message : "unknown"}`, "bad");
      }
    }
    activeSaveRef.current = false;
  };

  const toggleModel = (id: string, visible: boolean) => {
    const next = { ...localHiddenRef.current, [id]: !visible };
    localHiddenRef.current = next;
    setLocalHidden(next);
    const hidden = models.filter((m) => next[m.id] ?? m.hidden).map((m) => m.id);
    pendingTargetRef.current = { hiddenIds: hidden, localState: next };
    void drainMaskQueue();
  };

  const toggleProvider = async (provider: Provider, enabled: boolean) => {
    try {
      await api(`/admin/api/providers/${encodeURIComponent(provider.id)}`, { method: "PUT", body: { ...provider, enabled } });
      setProviders((prev) => prev.map((p) => (p.id === provider.id ? { ...p, enabled } : p)));
      toast(`${enabled ? "Enabled" : "Disabled"} ${provider.id}`);
    } catch (err) {
      toast(`Toggle failed: ${err instanceof Error ? err.message : "unknown"}`, "bad");
    }
  };

  const copyBaseUrl = async (provider: Provider) => {
    try {
      await navigator.clipboard.writeText(provider.base_url);
      toast(`Copied ${provider.name} base URL`);
    } catch (err) {
      toast(`Copy failed: ${err instanceof Error ? err.message : "clipboard unavailable"}`, "bad");
    }
  };

  const runTest = async (provider: Provider) => {
    setMenu(null);
    try {
      const result = await api<TestResult>(`/admin/api/providers/${encodeURIComponent(provider.id)}/test`, { method: "POST", body: {} });
      setTests((prev) => ({ ...prev, [provider.id]: result }));
      setBanner(
        result.ok ? null : `Provider test failed: ${provider.name}: ${formatProviderTestResult(result)}`,
        result.ok ? undefined : () => void runTest(provider),
        "provider-test",
      );
    } catch (err) {
      const result: TestResult = { ok: false, status: "error", error: err instanceof Error ? err.message : "Test failed" };
      setTests((prev) => ({ ...prev, [provider.id]: result }));
      setBanner(`Provider test failed: ${provider.name}: ${formatProviderTestResult(result)}`, () => void runTest(provider), "provider-test");
    }
  };

  const deleteProvider = async () => {
    if (!confirmDelete) return;
    setBusy(true);
    try {
      await api(`/admin/api/providers/${encodeURIComponent(confirmDelete.id)}`, { method: "DELETE" });
      toast(`Deleted provider ${confirmDelete.id}`);
      setConfirmDelete(null);
      await load();
    } catch (err) {
      toast(`Delete failed: ${err instanceof Error ? err.message : "unknown"}`, "bad");
    } finally {
      setBusy(false);
    }
  };

  const deleteModel = async () => {
    if (!confirmModel) return;
    setBusy(true);
    try {
      await api(`/admin/api/models/${modelPath(confirmModel)}`, { method: "DELETE" });
      toast(`Deleted ${confirmModel}`);
      setConfirmModel(null);
      await load();
    } catch (err) {
      toast(`Delete failed: ${err instanceof Error ? err.message : "unknown"}`, "bad");
    } finally {
      setBusy(false);
    }
  };

  const restore = async (id: string) => {
    try {
      await api(`/admin/api/models/${modelPath(id)}/restore`, { method: "POST" });
      toast(`Restored ${id}`);
      await load();
    } catch (err) {
      toast(`Restore failed: ${err instanceof Error ? err.message : "unknown"}`, "bad");
    }
  };

  const retryFailed = async () => {
    try {
      await api("/admin/api/models/retry-failed", { method: "POST" });
      toast("Retry queued");
      await load();
    } catch (err) {
      toast(`Retry failed: ${err instanceof Error ? err.message : "unknown"}`, "bad");
    }
  };

  const filtered = models.filter((m) => {
    const visible = !(localHidden[m.id] ?? m.hidden);
    if (filter === "on" && !visible) return false;
    if (filter === "off" && visible) return false;
    const q = query.toLowerCase();
    return !q || `${m.name} ${m.id} ${m.publisher || ""}`.toLowerCase().includes(q);
  });

  return (
    <>
      <PageHeader title="Models" sub="Providers and the model catalog">
        <HealthPill />
        <span class="text-xs text-dim">{onCount} on · {models.length - onCount} off</span>
        <button class="btn btn-primary" onClick={() => setEditor({ open: true, provider: null })}>Add provider</button>
      </PageHeader>

      <div class="grid grid-cols-[380px_1fr] items-start gap-3.5">
        <div>
          <div class="mb-2.5 flex items-center justify-between">
            <h2 class="text-sm font-semibold">Upstream providers</h2>
            <span class="text-xs text-dim">{providers.length} configured</span>
          </div>
          {loading ? <Card><Empty title="Loading…" body="Reading providers." /></Card> : providers.length === 0 ? (
            <Card><Empty title="No upstream providers" body="Add one to route gateway models to an upstream API." /></Card>
          ) : (
            providers.map((p) => (
              <div key={p.id} class="card relative mb-2 px-3 py-2.5">
                <div class="flex items-center gap-2">
                  <span class={`pill ${p.enabled ? "pill-ok" : "pill-mute"}`}>● {p.enabled ? "enabled" : "disabled"}</span>
                  <b class="text-[13px]">{p.name}</b>
                  <span class="chip">{p.protocol}</span>
                  <button
                    class="menu ml-auto grid size-[26px] place-items-center rounded-[7px] border border-line bg-panel text-dim"
                    aria-label={`Actions for ${p.id}`}
                    onClick={() => setMenu(menu === p.id ? null : p.id)}
                  >
                    <IconDots />
                  </button>
                </div>
                <div class="mt-1.5 flex items-start gap-2">
                  <span class="mono line-clamp-2 min-w-0 flex-1 break-all text-[11px] leading-4 text-dim" title={p.base_url}>{p.base_url}</span>
                  <button
                    class="btn btn-sm shrink-0"
                    aria-label={`Copy base URL for ${p.name}`}
                    title="Copy base URL"
                    onClick={() => void copyBaseUrl(p)}
                  >
                    Copy
                  </button>
                  <Switch checked={!!p.enabled} onChange={(v) => void toggleProvider(p, v)} label={`Enable ${p.id}`} />
                </div>
                <div class="mt-2 flex flex-wrap items-center gap-1.5">
                  {(p.models || []).map((m) => (
                    <span key={m.alias} class="chip" title={`Upstream: ${m.upstream_model}`}>
                      {m.alias}{m.hidden ? " (hidden)" : ""}
                    </span>
                  ))}
                  <span class="whitespace-nowrap text-[11px] text-dim">{(p.models || []).length} aliases</span>
                  {tests[p.id] ? (
                    <ProviderTestResultBadge result={tests[p.id]} className="ml-auto max-w-full justify-end" />
                  ) : (
                    <span class="ml-auto whitespace-nowrap text-[11px] text-dim">not tested</span>
                  )}
                </div>
                {menu === p.id && (
                  <div class="absolute right-2 top-10 z-10 w-[150px] rounded-lg border border-line2 bg-panel p-1 shadow-lg">
                    <MenuRow label="Edit provider" onClick={() => { setMenu(null); setEditor({ open: true, provider: p }); }} />
                    <MenuRow label="Test connection" onClick={() => void runTest(p)} />
                    <MenuRow label="Delete provider…" danger onClick={() => { setMenu(null); setConfirmDelete(p); }} />
                  </div>
                )}
              </div>
            ))
          )}
        </div>

        <Card>
          <CardHead title="Model catalog" sub={`${models.length} models`}>
            <div class="flex overflow-hidden rounded-lg border border-line text-[11px] font-semibold">
              {(["all", "on", "off"] as const).map((v) => (
                <button
                  key={v}
                  class={`px-2.5 py-1 ${filter === v ? "bg-accent-soft text-accent" : "text-dim"}`}
                  onClick={() => setFilter(v)}
                >
                  {v === "all" ? "All" : v === "on" ? "On" : "Off"}
                </button>
              ))}
            </div>
            <button class="btn btn-sm" onClick={() => void retryFailed()}>Retry failed</button>
          </CardHead>
          <div class="px-[15px] pb-2 pt-3">
            <input class="input" type="search" placeholder="Search name, id or publisher…" value={query} onInput={(e) => setQuery((e.target as HTMLInputElement).value)} />
          </div>
          {filtered.length === 0 ? (
            <Empty title="No models match" body="Change the search or filter." />
          ) : (
            <table class="table">
              <thead><tr><th class="w-11">On</th><th>Model</th><th>ID</th><th /></tr></thead>
              <tbody>
                {filtered.map((m) => {
                  const visible = !(localHidden[m.id] ?? m.hidden);
                  return (
                    <tr key={m.id}>
                      <td><Switch checked={visible} onChange={(v) => void toggleModel(m.id, v)} label={`Show ${m.name}`} /></td>
                      <td>
                        <b>{m.name}</b>
                        <div class="text-[11px] text-dim">{m.publisher || "custom"}</div>
                      </td>
                      <td class="mono text-dim">{m.id}</td>
                      <td class="text-right">
                        <button class="btn btn-sm" aria-label={`Delete ${m.id}`} onClick={() => setConfirmModel(m.id)}>Delete…</button>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
          <div class="border-t border-line">
            <button class="flex w-full items-center gap-2 px-[15px] py-2.5 text-xs text-dim" onClick={() => setDeletedOpen(!deletedOpen)}>
              <b class="text-xs text-fg">Deleted models</b>
              <span class="chip">{(catalog.deleted || []).length}</span>
              <span class="ml-auto">{deletedOpen ? "▾ hide" : "▸ show"}</span>
            </button>
            {deletedOpen && (
              <table class="table">
                <tbody>
                  {(catalog.deleted || []).map((id) => (
                    <tr key={id}>
                      <td class="mono text-dim">{id}</td>
                      <td class="text-right"><button class="btn btn-sm" onClick={() => void restore(id)}>Restore</button></td>
                    </tr>
                  ))}
                  {(catalog.deleted || []).length === 0 && (
                    <tr><td class="text-dim">No deleted models.</td></tr>
                  )}
                </tbody>
              </table>
            )}
          </div>
        </Card>
      </div>

      {editor?.open && (
        <ProviderEditor
          provider={editor.provider}
          onClose={() => setEditor(null)}
          onSaved={() => { setEditor(null); void load(); }}
        />
      )}
      {confirmDelete && (
        <Dialog
          title="Delete provider?"
          confirmLabel="Delete provider"
          danger
          busy={busy}
          onCancel={() => setConfirmDelete(null)}
          onConfirm={() => void deleteProvider()}
        >
          This removes <b class="text-fg">{confirmDelete.id}</b> and its {(confirmDelete.models || []).length} model mappings.
        </Dialog>
      )}
      {confirmModel && (
        <Dialog
          title="Delete model?"
          confirmLabel="Delete model"
          danger
          busy={busy}
          onCancel={() => setConfirmModel(null)}
          onConfirm={() => void deleteModel()}
        >
          This deletes <b class="text-fg">{confirmModel}</b> from the catalog; it can be restored from the Deleted models section.
        </Dialog>
      )}
    </>
  );
}

function MenuRow({ label, onClick, danger }: { label: string; onClick: () => void; danger?: boolean }) {
  return (
    <button class={`block w-full rounded-md px-2.5 py-1.5 text-left text-xs ${danger ? "text-bad" : "text-fg"} hover:bg-panel2`} onClick={onClick}>
      {label}
    </button>
  );
}
