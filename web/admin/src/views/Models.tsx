import { Fragment } from "preact";
import { useEffect, useState } from "preact/hooks";
import { api } from "../lib/api";
import { isSuperuserLevelRole, type AdminUser } from "../lib/auth";
import type { KeyModel, ModelInfo, ModelsResponse, NvidiaRescanResult, Provider, ProviderKey, TestResult } from "../lib/types";
import { buildUpstreamModel } from "../lib/helpers";
import { dateLabel, message } from "../lib/admin";
import {
  canDeleteKey,
  canEditKey,
  canEditKeyModels,
  canManageProviders,
  canTestKey,
  canToggleKey,
  copyBaseUrl,
  keyCreatePayload,
  keyCredentialLabel,
  keyDisablePayload,
  keyModelsPayload,
  keyOwnerLabel,
  keyReach,
  keyUpdatePayload,
  modelChoices,
  parseKeyList,
  parseKeyModels,
  toggleKeyModel,
} from "../lib/keys";
import {
  BaseUrlLine,
  Card,
  CardHead,
  Dialog,
  Empty,
  IconDots,
  IconX,
  PageHeader,
  Switch,
  useUi,
} from "../components/ui";
import { useGateway } from "../lib/context";
import { HealthPill } from "../components/Shell";
import { ModelApiGuide } from "../components/ApiGuide";
import { formatProviderTestResult, ProviderEditor, ProviderTestResultBadge } from "../components/ProviderEditor";

type KeyDialogState =
  | { kind: "create" }
  | { kind: "edit"; key: ProviderKey }
  | { kind: "delete"; key: ProviderKey }
  | null;

/** Superuser/admin-only trigger for a live Playground catalog rescan. */
export function NvidiaRescanButton({ role, busy, onClick }: { role: string; busy: boolean; onClick: () => void }) {
  if (!canManageProviders(role)) return null;
  return (
    <button class="btn" disabled={busy} onClick={onClick}>
      {busy ? "Rescanning…" : "Rescan NVIDIA models"}
    </button>
  );
}

export function rescanSummary(result: NvidiaRescanResult): string {
  return `${result.added} added, ${result.updated} updated, ${result.removed} removed`;
}

export function ModelsView({ user }: { user: AdminUser }) {
  const { toast, setBanner } = useUi();
  const { navSeq } = useGateway();
  const operator = canManageProviders(user.role);
  const [providers, setProviders] = useState<Provider[]>([]);
  const [loading, setLoading] = useState(true);
  const [selectedID, setSelectedID] = useState<string | null>(null);
  const [nvidiaOpen, setNvidiaOpen] = useState(false);
  const [nvidiaModels, setNvidiaModels] = useState<ModelInfo[]>([]);
  const [nvidiaLoading, setNvidiaLoading] = useState(false);
  const [callModel, setCallModel] = useState<string | null>(null);

  // Clicking Models in the nav returns to the provider list, even when the
  // view is already on screen with a provider open.
  useEffect(() => {
    setSelectedID(null);
    setNvidiaOpen(false);
  }, [navSeq]);
  const [menu, setMenu] = useState<string | null>(null);
  const [editor, setEditor] = useState<{ open: boolean; provider: Provider | null } | null>(null);
  const [confirmDelete, setConfirmDelete] = useState<Provider | null>(null);
  const [busy, setBusy] = useState(false);
  const [tests, setTests] = useState<Record<string, TestResult>>({});
  const [rescanBusy, setRescanBusy] = useState(false);
  const [rescanResult, setRescanResult] = useState<NvidiaRescanResult | null>(null);
  const [rescanError, setRescanError] = useState<string | null>(null);

  const runRescan = async () => {
    setRescanBusy(true);
    setRescanError(null);
    try {
      const result = await api<NvidiaRescanResult>("/admin/api/nvidia/models/rescan", { method: "POST", body: {} });
      setRescanResult(result);
      await load();
      // The rescan rewrites the catalog, so the models on screen are stale
      // until they are read again.
      if (nvidiaOpen) await loadNvidiaModels();
    } catch (err) {
      setRescanResult(null);
      setRescanError(message(err));
    } finally {
      setRescanBusy(false);
    }
  };

  const load = async () => {
    try {
      const list = await api<Provider[]>("/admin/api/providers");
      setProviders(list);
      setLoading(false);
      setBanner(null, undefined, "models");
    } catch (err) {
      setLoading(false);
      setBanner(`Providers failed to load: ${message(err)}`, () => void load(), "models");
    }
  };

  useEffect(() => {
    void load();
  }, []);

  const selected = selectedID ? providers.find((p) => p.id === selectedID) ?? null : null;

  const copyProviderBaseUrl = async (provider: Provider) => {
    const ok = await copyBaseUrl(provider.base_url);
    toast(ok ? `Copied ${provider.name} base URL` : "Copy failed: clipboard unavailable", ok ? "ok" : "bad");
  };

  const toggleProvider = async (provider: Provider, enabled: boolean) => {
    try {
      await api(`/admin/api/providers/${encodeURIComponent(provider.id)}`, { method: "PUT", body: { ...provider, enabled } });
      setProviders((prev) => prev.map((p) => (p.id === provider.id ? { ...p, enabled } : p)));
      toast(`${enabled ? "Enabled" : "Disabled"} ${provider.id}`);
    } catch (err) {
      toast(`Toggle failed: ${message(err)}`, "bad");
    }
  };

  const runProviderTest = async (provider: Provider) => {
    setMenu(null);
    try {
      const result = await api<TestResult>(`/admin/api/providers/${encodeURIComponent(provider.id)}/test`, { method: "POST", body: {} });
      setTests((prev) => ({ ...prev, [provider.id]: result }));
      setBanner(
        result.ok ? null : `Provider test failed: ${provider.name}: ${formatProviderTestResult(result)}`,
        result.ok ? undefined : () => void runProviderTest(provider),
        "provider-test",
      );
    } catch (err) {
      const result: TestResult = { ok: false, status: "error", error: message(err) };
      setTests((prev) => ({ ...prev, [provider.id]: result }));
      setBanner(`Provider test failed: ${provider.name}: ${formatProviderTestResult(result)}`, () => void runProviderTest(provider), "provider-test");
    }
  };

  const deleteProvider = async () => {
    if (!confirmDelete) return;
    setBusy(true);
    try {
      await api(`/admin/api/providers/${encodeURIComponent(confirmDelete.id)}`, { method: "DELETE" });
      toast(`Deleted provider ${confirmDelete.id}`);
      setConfirmDelete(null);
      setSelectedID(null);
      await load();
    } catch (err) {
      toast(`Delete failed: ${message(err)}`, "bad");
    } finally {
      setBusy(false);
    }
  };

  const loadNvidiaModels = async () => {
    setNvidiaLoading(true);
    try {
      const body = await api<ModelsResponse>("/admin/api/models");
      setNvidiaModels((body.models ?? []).filter((model) => !model.custom));
    } catch (err) {
      toast(`NVIDIA models unavailable: ${message(err)}`, "bad");
    } finally {
      setNvidiaLoading(false);
    }
  };

  const openNvidia = async () => {
    setNvidiaOpen(true);
    await loadNvidiaModels();
  };

  if (callModel) {
    return <ModelCallView model={callModel} onBack={() => setCallModel(null)} />;
  }

  if (nvidiaOpen) {
    return (
      <>
        <PageHeader title="NVIDIA Playground" sub="Shared Chrome captcha pool · no per-user key">
          <HealthPill />
          <NvidiaRescanButton role={user.role} busy={rescanBusy} onClick={() => void runRescan()} />
          <button class="btn" onClick={() => setNvidiaOpen(false)}>All providers</button>
        </PageHeader>
        {operator && (rescanResult || rescanError) && (
          <div
            class={`mb-3 rounded-lg border px-3 py-2 text-xs ${rescanError ? "border-bad bg-bad-soft text-bad" : "border-line text-dim"}`}
            role={rescanError ? "alert" : "status"}
          >
            {rescanError
              ? `Rescan failed: ${rescanError}`
              : `Rescan complete: ${rescanSummary(rescanResult!)} · ${new Date(rescanResult!.scanned_at).toLocaleString()}`}
          </div>
        )}
        <Card>
          <CardHead title="Models" sub={`${nvidiaModels.length} available`} />
          {nvidiaLoading ? (
            <Empty title="Loading…" body="Reading the NVIDIA catalog." />
          ) : nvidiaModels.length === 0 ? (
            <Empty title="No NVIDIA models" body={operator ? "Run a rescan to load the Playground catalog." : "No Playground models are available right now."} />
          ) : (
            <table class="table">
              <thead>
                <tr><th>Model</th><th>Publisher</th><th>Status</th><th /></tr>
              </thead>
              <tbody>
                {nvidiaModels.map((model) => (
                  <tr key={model.id}>
                    <td>
                      <button type="button" class="mono text-left text-[12px] hover:text-accent" aria-label={`Call ${model.id}`} onClick={() => setCallModel(model.id)}>
                        {model.id}
                      </button>
                    </td>
                    <td class="muted">{model.publisher || "—"}</td>
                    <td class="muted">{model.hidden ? "hidden" : "available"}</td>
                    <td class="whitespace-nowrap text-right">
                      <button class="btn btn-sm" onClick={() => setCallModel(model.id)}>Call</button>
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

  if (selected) {
    return (
      <ProviderKeys
        provider={selected}
        user={user}
        onBack={() => setSelectedID(null)}
        onCall={setCallModel}
        onCopyBaseUrl={() => void copyProviderBaseUrl(selected)}
      />
    );
  }

  return (
    <>
      <PageHeader title="Models" sub="Providers, their API keys and per-key model selection">
        <HealthPill />
        <span class="text-xs text-dim">{providers.length} providers</span>
        <button class="btn" onClick={() => setCallModel("automode")}>Call API</button>
        <NvidiaRescanButton role={user.role} busy={rescanBusy} onClick={() => void runRescan()} />
        {operator && <button class="btn btn-primary" onClick={() => setEditor({ open: true, provider: null })}>Add provider</button>}
      </PageHeader>

      {operator && (rescanResult || rescanError) && (
        <div
          class={`mb-3 rounded-lg border px-3 py-2 text-xs ${rescanError ? "border-bad bg-bad-soft text-bad" : "border-line text-dim"}`}
          role={rescanError ? "alert" : "status"}
        >
          {rescanError
            ? `Rescan failed: ${rescanError}`
            : `Rescan complete: ${rescanSummary(rescanResult!)} · ${new Date(rescanResult!.scanned_at).toLocaleString()}`}
        </div>
      )}

      <div class="mb-2.5 flex items-center justify-between">
        <h2 class="text-sm font-semibold">Upstream providers</h2>
        <span class="text-xs text-dim">Select a provider to manage its keys</span>
      </div>
      {loading ? (
        <Card><Empty title="Loading…" body="Reading providers." /></Card>
      ) : (
        <div class="grid gap-2.5">
          <div class="card relative px-3 py-2.5">
            <div class="flex items-center gap-2">
              <span class="pill pill-ok">● shared</span>
              <b class="text-[13px]">NVIDIA Playground</b>
              <span class="chip">captcha</span>
            </div>
            <div class="mt-1.5 text-[11px] text-dim">
              Built-in models served through the shared Chrome captcha pool; no per-user key.
            </div>
            <div class="mt-2 flex flex-wrap items-center gap-1.5">
              <button class="btn btn-sm" onClick={() => void openNvidia()}>View models</button>
            </div>
          </div>
          {providers.length === 0 && (
            <Card><Empty title="No upstream providers" body={operator ? "Add one to route gateway models to an upstream API." : "No additional providers are available to your account."} /></Card>
          )}
          {providers.map((provider) => (
            <div key={provider.id} class="card relative px-3 py-2.5">
              <div class="flex items-center gap-2">
                <span class={`pill ${provider.enabled ? "pill-ok" : "pill-mute"}`}>● {provider.enabled ? "enabled" : "disabled"}</span>
                <b class="text-[13px]">{provider.name}</b>
                <span class="chip">{provider.protocol}</span>
                {operator && (
                  <button
                    class="menu ml-auto grid size-[26px] place-items-center rounded-[7px] border border-line bg-panel text-dim"
                    aria-label={`Actions for ${provider.id}`}
                    onClick={() => setMenu(menu === provider.id ? null : provider.id)}
                  >
                    <IconDots />
                  </button>
                )}
              </div>
              {provider.base_url && (
                <div class="mt-1.5">
                  <BaseUrlLine url={provider.base_url} onCopy={() => void copyProviderBaseUrl(provider)} />
                </div>
              )}
              <div class="mt-2 flex flex-wrap items-center gap-1.5">
                <button class="btn btn-sm" onClick={() => setSelectedID(provider.id)}>Manage keys</button>
                <span class="text-[11px] text-dim">{(provider.models || []).length} model aliases</span>
                {operator && <Switch checked={!!provider.enabled} onChange={(v) => void toggleProvider(provider, v)} label={`Enable ${provider.id}`} />}
                {tests[provider.id] && <ProviderTestResultBadge result={tests[provider.id]} className="ml-auto max-w-full justify-end" />}
              </div>
              {operator && menu === provider.id && (
                <div class="absolute right-2 top-10 z-10 w-[150px] rounded-lg border border-line2 bg-panel p-1 shadow-lg">
                  <MenuRow label="Edit provider" onClick={() => { setMenu(null); setEditor({ open: true, provider }); }} />
                  <MenuRow label="Test connection" onClick={() => void runProviderTest(provider)} />
                  <MenuRow label="Delete provider…" danger onClick={() => { setMenu(null); setConfirmDelete(provider); }} />
                </div>
              )}
            </div>
          ))}
        </div>
      )}

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
    </>
  );
}

function ProviderKeys({
  provider,
  user,
  onBack,
  onCall,
  onCopyBaseUrl,
}: {
  provider: Provider;
  user: AdminUser;
  onBack: () => void;
  onCall: (alias: string) => void;
  onCopyBaseUrl: () => void;
}) {
  const { toast, setBanner } = useUi();
  const base = `/admin/api/providers/${encodeURIComponent(provider.id)}/keys`;
  const keyPath = (key: ProviderKey) => `${base}/${encodeURIComponent(key.id)}`;
  const [keys, setKeys] = useState<ProviderKey[]>([]);
  const [loading, setLoading] = useState(true);
  const [dialog, setDialog] = useState<KeyDialogState>(null);
  const [busy, setBusy] = useState(false);
  const [tests, setTests] = useState<Record<string, TestResult>>({});
  const [expanded, setExpanded] = useState<string | null>(null);
  const [keyModels, setKeyModels] = useState<Record<string, KeyModel[]>>({});
  const [modelsLoading, setModelsLoading] = useState<string | null>(null);
  const [reveal, setReveal] = useState<{
    key: ProviderKey;
    to: string;
    otp: string;
    secret: string | null;
    error: string | null;
    busy: boolean;
  } | null>(null);

  const load = async () => {
    setLoading(true);
    try {
      setKeys(parseKeyList(await api<unknown>(base)));
      setBanner(null, undefined, `keys:${provider.id}`);
    } catch (err) {
      setBanner(`Keys unavailable: ${message(err)}`, () => void load(), `keys:${provider.id}`);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void load();
  }, [provider.id]);

  const runTest = async (key: ProviderKey) => {
    try {
      const result = await api<TestResult>(`${keyPath(key)}/test`, { method: "POST", body: {} });
      setTests((prev) => ({ ...prev, [key.id]: result }));
      setBanner(
        result.ok ? null : `Key test failed: ${key.name}: ${formatProviderTestResult(result)}`,
        result.ok ? undefined : () => void runTest(key),
        `key-test:${key.id}`,
      );
    } catch (err) {
      const result: TestResult = { ok: false, status: "error", error: message(err) };
      setTests((prev) => ({ ...prev, [key.id]: result }));
      setBanner(`Key test failed: ${key.name}: ${formatProviderTestResult(result)}`, () => void runTest(key), `key-test:${key.id}`);
    }
  };

  /** An admin may only disable a foreign key; everyone else toggles their own. */
  const setKeyEnabled = async (key: ProviderKey, enabled: boolean) => {
    const reach = keyReach(user, key);
    if (!canToggleKey(reach)) return;
    try {
      await api(keyPath(key), {
        method: "PUT",
        body: reach === "full" ? { enabled } : keyDisablePayload(),
      });
      toast(`${enabled ? "Enabled" : "Disabled"} ${key.name}`);
      await load();
    } catch (err) {
      toast(`Update failed: ${message(err)}`, "bad");
    }
  };

  const loadModels = async (key: ProviderKey) => {
    if (expanded === key.id) {
      setExpanded(null);
      return;
    }
    setExpanded(key.id);
    if (keyModels[key.id]) return;
    setModelsLoading(key.id);
    try {
      const body = await api<unknown>(`${keyPath(key)}/models`);
      setKeyModels((prev) => ({ ...prev, [key.id]: parseKeyModels(body) }));
    } catch (err) {
      toast(`Models unavailable: ${message(err)}`, "bad");
      setExpanded(null);
    } finally {
      setModelsLoading(null);
    }
  };

  const requestReveal = async (key: ProviderKey) => {
    setReveal({ key, to: "", otp: "", secret: null, error: null, busy: true });
    try {
      const res = await api<{ to?: string }>(`${keyPath(key)}/reveal/request`, { method: "POST", body: {} });
      setReveal({ key, to: res.to || "your email", otp: "", secret: null, error: null, busy: false });
    } catch (err) {
      toast(`Reveal failed: ${message(err)}`, "bad");
      setReveal(null);
    }
  };

  const verifyReveal = async () => {
    if (!reveal || reveal.secret !== null) return;
    const otp = reveal.otp.trim();
    if (!otp) {
      setReveal({ ...reveal, error: "Enter the 6-digit verification code" });
      return;
    }
    setReveal({ ...reveal, error: null, busy: true });
    try {
      const res = await api<{ secret: string }>(`${keyPath(reveal.key)}/reveal`, { method: "POST", body: { otp } });
      setReveal({ ...reveal, secret: res.secret, error: null, busy: false });
    } catch (err) {
      setReveal({ ...reveal, error: message(err), busy: false });
    }
  };

  const deleteKey = async () => {
    if (dialog?.kind !== "delete") return;
    setBusy(true);
    try {
      await api(keyPath(dialog.key), { method: "DELETE" });
      toast(`Deleted key ${dialog.key.name}`);
      setDialog(null);
      await load();
    } catch (err) {
      toast(`Delete failed: ${message(err)}`, "bad");
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <PageHeader title={provider.name} sub={`API keys · ${provider.id}`}>
        <HealthPill />
        <button class="btn" onClick={onBack}>All providers</button>
      </PageHeader>
      {provider.base_url && (
        <div class="mb-3">
          <BaseUrlLine url={provider.base_url} onCopy={onCopyBaseUrl} />
        </div>
      )}

      <Card>
        <CardHead title="API keys" sub={`${keys.length} keys`}>
          <button class="btn btn-primary btn-sm" onClick={() => setDialog({ kind: "create" })}>New key</button>
        </CardHead>
        {loading && keys.length === 0 ? (
          <Empty title="Loading…" body="Reading provider keys." />
        ) : keys.length === 0 ? (
          <Empty title="No keys" body="Add a key to route this provider's models for an account." />
        ) : (
          <table class="table">
            <thead>
              <tr>
                <th>Key</th>
                <th>Auth</th>
                <th>Credential</th>
                <th>Status</th>
                <th>Created</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {keys.map((key) => {
                const reach = keyReach(user, key);
                const canToggle = canToggleKey(reach);
                return (
                  <Fragment key={key.id}>
                    <tr>
                      <td>
                        <div class="font-semibold">{key.name}</div>
                        <div class="text-[11px] text-dim">
                          {keyOwnerLabel(user, key)} · <span class="mono">{key.id.slice(0, 8)}…</span>
                        </div>
                        {tests[key.id] && <ProviderTestResultBadge result={tests[key.id]} className="mt-1" />}
                      </td>
                      <td class="mono muted">{keyCredentialLabel(key)}</td>
                      <td>
                        <Switch
                          checked={key.enabled}
                          disabled={!canToggle || (reach === "disable" && !key.enabled)}
                          onChange={(v) => void setKeyEnabled(key, v)}
                          label={`Enable ${key.name}`}
                        />
                      </td>
                      <td class="muted">{dateLabel(key.created_at)}</td>
                      <td class="whitespace-nowrap text-right">
                        {canEditKey(reach) && (
                          <button class="btn btn-sm" onClick={() => setDialog({ kind: "edit", key })}>Edit</button>
                        )}{" "}
                        {canEditKey(reach) && key.has_secret && (
                          <button class="btn btn-sm" onClick={() => void requestReveal(key)}>Show</button>
                        )}{" "}
                        {canTestKey(reach) && key.auth_scheme !== "none" && (
                          <button class="btn btn-sm" onClick={() => void runTest(key)}>Test</button>
                        )}{" "}
                        {canEditKeyModels(reach) && (
                          <button class="btn btn-sm" onClick={() => void loadModels(key)}>
                            {expanded === key.id ? "Hide models" : "Models"}
                          </button>
                        )}{" "}
                        {canDeleteKey(reach) && (
                          <button class="btn btn-sm btn-danger" onClick={() => setDialog({ kind: "delete", key })}>Delete</button>
                        )}
                      </td>
                    </tr>
                    {expanded === key.id && (
                      <tr>
                        <td colSpan={6} class="bg-panel2">
                          {modelsLoading === key.id ? (
                            <span class="text-xs text-dim">Loading models…</span>
                          ) : (
                            <KeyModelsPanel
                              endpoint={`${keyPath(key)}/models`}
                              provider={provider}
                              initial={keyModels[key.id] ?? []}
                              onCall={onCall}
                              onSaved={(next) => {
                                setKeyModels((prev) => ({ ...prev, [key.id]: next }));
                                toast(`Models saved for ${key.name}`);
                              }}
                            />
                          )}
                        </td>
                      </tr>
                    )}
                  </Fragment>
                );
              })}
            </tbody>
          </table>
        )}
      </Card>

      {dialog?.kind === "create" && (
        <KeyEditorDialog
          provider={provider}
          target={null}
          user={user}
          onClose={() => setDialog(null)}
          onSaved={() => { setDialog(null); toast("Key created"); void load(); }}
        />
      )}
      {reveal && (
        <Dialog
          title={`Show key · ${reveal.key.name}`}
          confirmLabel={reveal.secret !== null ? "Copy" : "Verify"}
          busy={reveal.busy}
          onCancel={() => setReveal(null)}
          onConfirm={() => {
            if (reveal.secret !== null) {
              void navigator.clipboard.writeText(reveal.secret).then(
                () => toast("Copied"),
                () => toast("Copy failed", "bad"),
              );
            } else {
              void verifyReveal();
            }
          }}
        >
          {reveal.secret !== null ? (
            <div class="space-y-2">
              <div class="text-xs text-dim">Decrypted credential for <b class="text-fg">{reveal.key.name}</b>:</div>
              <pre class="mono overflow-x-auto rounded-lg border border-line bg-panel2 px-3 py-2 text-xs select-all">{reveal.secret}</pre>
            </div>
          ) : (
            <div class="space-y-2">
              <div class="text-xs text-dim">
                {reveal.to ? <>Enter the 6-digit OTP sent to <b class="text-fg">{reveal.to}</b>.</> : "Sending verification code…"}
              </div>
              <input
                class="input mono w-full"
                type="text"
                inputMode="numeric"
                maxLength={6}
                placeholder="123456"
                value={reveal.otp}
                disabled={reveal.busy}
                onInput={(e) => setReveal({ ...reveal, otp: (e.target as HTMLInputElement).value })}
              />
              {reveal.error && <div class="text-xs text-bad">{reveal.error}</div>}
            </div>
          )}
        </Dialog>
      )}
      {dialog?.kind === "edit" && (
        <KeyEditorDialog
          provider={provider}
          target={dialog.key}
          user={user}
          onClose={() => setDialog(null)}
          onSaved={() => { setDialog(null); toast("Key updated"); void load(); }}
        />
      )}
      {dialog?.kind === "delete" && (
        <Dialog
          title={`Delete key “${dialog.key.name}”?`}
          confirmLabel="Delete key"
          danger
          busy={busy}
          onCancel={() => setDialog(null)}
          onConfirm={() => void deleteKey()}
        >
          This removes the key and its model assignments. Requests routed through it will start failing.
        </Dialog>
      )}
    </>
  );
}

function KeyEditorDialog({
  provider,
  target,
  user,
  onClose,
  onSaved,
}: {
  provider: Provider;
  target: ProviderKey | null;
  user: AdminUser;
  onClose: () => void;
  onSaved: () => void;
}) {
  const canShare = target === null && isSuperuserLevelRole(user.role);
  const [name, setName] = useState(target?.name ?? "");
  const [enabled, setEnabled] = useState(target?.enabled ?? true);
  const [authKey, setAuthKey] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [shared, setShared] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [rows, setRows] = useState<KeyModel[] | null>(target ? null : []);
  const [modelAlias, setModelAlias] = useState("");

  const base = `/admin/api/providers/${encodeURIComponent(provider.id)}/keys`;

  useEffect(() => {
    if (!target) return;
    let live = true;
    void api<unknown>(`${base}/${encodeURIComponent(target.id)}/models`)
      .then((body) => { if (live) setRows(parseKeyModels(body)); })
      .catch(() => { if (live) setRows([]); });
    return () => { live = false; };
  }, [target, base]);

  const addModel = () => {
    if (!modelAlias.trim()) return;
    setRows(toggleKeyModel(rows ?? [], { alias: modelAlias.trim(), upstream_model: buildUpstreamModel(modelAlias.trim(), "") }, true));
    setModelAlias("");
  };

  const trimmedAuth = authKey.trim();
  const trimmedAPI = apiKey.trim();
  // Auth key -> bearer; API key -> x-api-key header. One credential per key.
  const effectiveScheme = shared ? "none" : trimmedAuth ? "bearer" : trimmedAPI ? "header" : target?.auth_scheme ?? "bearer";
  const showCredential = effectiveScheme !== "none";
  // Both credentials travel in one encrypted secret; the server sends auth_key
  // as Authorization: Bearer and api_key to the header named here.
  const secretValue = trimmedAuth && trimmedAPI
    ? JSON.stringify({ auth_key: trimmedAuth, api_key: trimmedAPI, api_key_header: "x-api-key" })
    : trimmedAuth || trimmedAPI || undefined;

  const submit = async () => {
    setError(null);
    if (!shared && !target && !trimmedAuth && !trimmedAPI) {
      setError("Fill at least one of Auth key or API key");
      return;
    }
    setBusy(true);
    try {
      const input = {
        name,
        enabled,
        authScheme: effectiveScheme,
        // A paired api_key names its own header inside the secret, so the key
        // column stays reserved for header-scheme keys.
        authHeaderName: shared ? undefined : trimmedAPI && !trimmedAuth ? "x-api-key" : target?.auth_scheme === "header" ? target.auth_header_name : undefined,
        secret: secretValue,
      };
      let keyID = target?.id;
      if (target) {
        await api(`${base}/${encodeURIComponent(target.id)}`, {
          method: "PUT",
          body: keyUpdatePayload(input),
        });
      } else {
        const created = await api<unknown>(base, { method: "POST", body: keyCreatePayload(input, { shared }) });
        const id = (created as { id?: unknown } | null)?.id;
        keyID = typeof id === "string" ? id : undefined;
      }
      if (keyID && rows) {
        await api(`${base}/${encodeURIComponent(keyID)}/models`, { method: "PUT", body: keyModelsPayload(rows) });
      }
      onSaved();
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      title={target ? `Edit key · ${target.name}` : "New key"}
      confirmLabel={target ? "Save" : "Create key"}
      onConfirm={() => void submit()}
      onCancel={onClose}
      busy={busy}
    >
      <div class="space-y-3">
        <label class="block text-xs font-semibold">
          Name
          <input class="input mt-1" value={name} disabled={busy} autoFocus
            onInput={(e) => setName((e.currentTarget as HTMLInputElement).value)} />
        </label>
        <label class="flex items-center gap-2 text-xs font-semibold">
          <Switch checked={enabled} onChange={setEnabled} label="Key enabled" />
          Enabled
        </label>
        {canShare && (
          <label class="flex items-center gap-2 text-xs font-semibold">
            <input type="checkbox" checked={shared} disabled={busy} onChange={(e) => setShared(e.currentTarget.checked)} />
            <span>
              Shared key (no owner)
              <span class="mt-0.5 block text-[11px] font-normal text-dim">Shared keys use auth none and serve every account.</span>
            </span>
          </label>
        )}
        {showCredential && (
          <>
            <label class="block text-xs font-semibold">
              Auth key
              <input class="input mt-1" type="password" autoComplete="off" value={authKey} disabled={busy}
                placeholder={target?.has_secret ? "leave blank to keep the stored key" : "sent as Authorization: Bearer"}
                onInput={(e) => setAuthKey((e.currentTarget as HTMLInputElement).value)} />
            </label>
            <label class="block text-xs font-semibold">
              API key
              <input class="input mt-1" type="password" autoComplete="off" value={apiKey} disabled={busy}
                placeholder="sent as x-api-key"
                onInput={(e) => setApiKey((e.currentTarget as HTMLInputElement).value)} />
            </label>
            <span class="block text-[11px] font-normal text-dim">
              Fill either or both. Sent as Authorization: Bearer (auth key) and x-api-key (api key). Editing with both blank keeps the stored credential. Write-only: only the last four characters are shown later.
            </span>
          </>
        )}
        {rows !== null && (
          <div class="space-y-2 border-t border-line pt-3">
            <b class="text-xs">Models for this key</b>
            {rows.map((item) => (
              <div key={item.alias} class="flex flex-wrap items-center gap-2">
                <code class="mono w-[150px] truncate text-[11px]" title={item.alias}>{item.alias}</code>
                <input
                  class="input mono h-7 w-[190px]"
                  aria-label={`Upstream model for ${item.alias}`}
                  value={item.upstream_model}
                  placeholder={item.alias.endsWith("/*") ? "(suffix)" : "upstream model"}
                  onInput={(e) =>
                    setRows(rows.map((row) =>
                      row.alias === item.alias ? { ...row, upstream_model: (e.currentTarget as HTMLInputElement).value } : row,
                    ))
                  }
                />
                <button class="btn btn-sm" aria-label={`Remove ${item.alias}`}
                  onClick={() => setRows(rows.filter((row) => row.alias !== item.alias))}>
                  <IconX />
                </button>
              </div>
            ))}
            <div class="flex flex-wrap items-center gap-2">
              <input class="input mono h-7 w-[240px]" aria-label="New model" placeholder="model name, press Enter to add" value={modelAlias}
                onInput={(e) => setModelAlias((e.currentTarget as HTMLInputElement).value)}
                onKeyDown={(e) => { if (e.key === "Enter") addModel(); }} />
              <button class="btn btn-sm" disabled={!modelAlias.trim()} onClick={addModel}>Add model</button>
            </div>
            <span class="block text-[11px] font-normal text-dim">
              Aliases chosen here belong to this key only.
            </span>
          </div>
        )}
        {error && <div role="alert" class="rounded-lg border border-bad bg-bad-soft px-3 py-2 text-xs text-bad">{error}</div>}
      </div>
    </Dialog>
  );
}

function KeyModelsPanel({
  endpoint,
  provider,
  initial,
  onSaved,
  onCall,
}: {
  endpoint: string;
  provider: Provider;
  initial: KeyModel[];
  onSaved: (models: KeyModel[]) => void;
  onCall: (alias: string) => void;
}) {
  const [rows, setRows] = useState<KeyModel[]>(initial);
  const [alias, setAlias] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState<string | null>(null);
  const [draft, setDraft] = useState<Partial<KeyModel>>({});
  const choices = modelChoices(provider.models, rows);

  // Every change is persisted immediately; Enter commits the row being edited.
  const persist = async (next: KeyModel[]) => {
    setBusy(true);
    setError(null);
    try {
      const saved = parseKeyModels(await api<unknown>(endpoint, { method: "PUT", body: keyModelsPayload(next) }));
      setRows(saved);
      onSaved(saved);
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy(false);
    }
  };

  const add = (value: string, upstreamModel: string) => {
    const model = value.trim();
    if (!model) return;
    setAlias("");
    void persist(toggleKeyModel(rows, { alias: model, upstream_model: buildUpstreamModel(model, upstreamModel) }, true));
  };

  const startEdit = (item: KeyModel) => {
    setEditing(item.alias);
    setDraft({ upstream_model: item.upstream_model, hidden: item.hidden });
  };

  const commitEdit = () => {
    if (!editing) return;
    const next = rows.map((row) => (row.alias === editing ? { ...row, ...draft } : row));
    setEditing(null);
    setDraft({});
    void persist(next);
  };

  return (
    <div class="space-y-2 py-1">
      <div class="flex items-center gap-2">
        <b class="text-xs">Models for this key</b>
        <span class="text-[11px] text-dim">{rows.length} selected</span>
        {busy && <span class="text-[11px] text-dim">Saving…</span>}
      </div>
      {rows.map((item) =>
        editing === item.alias ? (
          <div key={item.alias} class="flex flex-wrap items-center gap-2">
            <code class="mono w-[180px] truncate text-[11px]" title={item.alias}>{item.alias}</code>
            <input
              class="input mono h-7 w-[220px]"
              aria-label={`Upstream model for ${item.alias}`}
              value={draft.upstream_model ?? ""}
              placeholder={item.alias.endsWith("/*") ? "(suffix)" : "upstream model"}
              autoFocus
              onInput={(e) => setDraft((d) => ({ ...d, upstream_model: (e.currentTarget as HTMLInputElement).value }))}
              onKeyDown={(e) => {
                if (e.key === "Enter") commitEdit();
                if (e.key === "Escape") { setEditing(null); setDraft({}); }
              }}
            />
            <label class="flex items-center gap-1.5 text-[11px] text-dim">
              <Switch checked={draft.hidden ?? false} onChange={(v) => setDraft((d) => ({ ...d, hidden: v }))} label={`Hide ${item.alias}`} />
              hidden
            </label>
            <button class="btn btn-sm" disabled={busy} onClick={commitEdit}>Save</button>
          </div>
        ) : (
          <Fragment key={item.alias}>
            <div class="flex flex-wrap items-center gap-2">
              <code class="mono w-[180px] truncate text-[11px]" title={item.alias}>{item.alias}</code>
              <span class="mono w-[220px] truncate text-[11px] text-dim" title={item.upstream_model}>{item.upstream_model}</span>
              {item.hidden && <span class="text-[11px] text-dim">hidden</span>}
              <button class="btn btn-sm" disabled={busy} aria-label={`Edit ${item.alias}`} onClick={() => startEdit(item)}>Edit</button>
              <button class="btn btn-sm" aria-label={`Call ${item.alias}`} onClick={() => onCall(item.alias)}>Call</button>
              <button class="btn btn-sm" disabled={busy} aria-label={`Remove ${item.alias}`} onClick={() => void persist(rows.filter((row) => row.alias !== item.alias))}>
                <IconX />
              </button>
            </div>
          </Fragment>
        ),
      )}
      {rows.length === 0 && <div class="text-[11px] text-dim">No models selected for this key.</div>}
      <div class="flex flex-wrap items-center gap-2 border-t border-line pt-2">
        <input class="input mono h-7 w-[240px]" aria-label="New model" placeholder="model name, press Enter to save" value={alias}
          disabled={busy}
          onInput={(e) => setAlias((e.currentTarget as HTMLInputElement).value)}
          onKeyDown={(e) => { if (e.key === "Enter") add(alias, ""); }} />
        <button class="btn btn-sm" disabled={!alias.trim() || busy} onClick={() => add(alias, "")}>Add model</button>
      </div>
      {choices.length > 0 && (
        <div class="flex flex-wrap items-center gap-1.5">
          <span class="text-[11px] text-dim">Available:</span>
          {choices.map((choice) => (
            <button key={choice.alias} class="chip" type="button" onClick={() => add(choice.alias, choice.upstream_model)}>
              + {choice.alias}
            </button>
          ))}
        </div>
      )}
      {error && <div role="alert" class="rounded-lg border border-bad bg-bad-soft px-3 py-2 text-xs text-bad">{error}</div>}
    </div>
  );
}

/** Full-width call view: the shared guide for one model alias. */
function ModelCallView({ model, onBack }: { model: string; onBack: () => void }) {
  return (
    <>
      <PageHeader title={`Call ${model}`} sub="Sample code and agent setup for this model">
        <button class="btn" onClick={onBack}>Back</button>
      </PageHeader>
      <Card>
        <CardHead title="Sample code" sub="Switch the protocol, then copy any block." />
        <div class="px-[15px] py-3.5">
          <ModelApiGuide model={model} />
        </div>
      </Card>
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
