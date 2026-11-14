import { Fragment } from "preact";
import { useEffect, useState } from "preact/hooks";
import { api } from "../lib/api";
import type { AdminUser } from "../lib/auth";
import type { KeyModel, Provider, ProviderKey, TestResult } from "../lib/types";
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
import { HealthPill } from "../components/Shell";
import { formatProviderTestResult, ProviderEditor, ProviderTestResultBadge } from "../components/ProviderEditor";

type KeyDialogState =
  | { kind: "create" }
  | { kind: "edit"; key: ProviderKey }
  | { kind: "delete"; key: ProviderKey }
  | null;

export function ModelsView({ user }: { user: AdminUser }) {
  const { toast, setBanner } = useUi();
  const operator = canManageProviders(user.role);
  const [providers, setProviders] = useState<Provider[]>([]);
  const [loading, setLoading] = useState(true);
  const [selectedID, setSelectedID] = useState<string | null>(null);
  const [menu, setMenu] = useState<string | null>(null);
  const [editor, setEditor] = useState<{ open: boolean; provider: Provider | null } | null>(null);
  const [confirmDelete, setConfirmDelete] = useState<Provider | null>(null);
  const [busy, setBusy] = useState(false);
  const [tests, setTests] = useState<Record<string, TestResult>>({});

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

  if (selected) {
    return (
      <ProviderKeys
        provider={selected}
        user={user}
        onBack={() => setSelectedID(null)}
        onCopyBaseUrl={() => void copyProviderBaseUrl(selected)}
      />
    );
  }

  return (
    <>
      <PageHeader title="Models" sub="Providers, their API keys and per-key model selection">
        <HealthPill />
        <span class="text-xs text-dim">{providers.length} providers</span>
        {operator && <button class="btn btn-primary" onClick={() => setEditor({ open: true, provider: null })}>Add provider</button>}
      </PageHeader>

      <div class="mb-2.5 flex items-center justify-between">
        <h2 class="text-sm font-semibold">Upstream providers</h2>
        <span class="text-xs text-dim">Select a provider to manage its keys</span>
      </div>
      {loading ? (
        <Card><Empty title="Loading…" body="Reading providers." /></Card>
      ) : providers.length === 0 ? (
        <Card><Empty title="No upstream providers" body={operator ? "Add one to route gateway models to an upstream API." : "No enabled providers are available to your account."} /></Card>
      ) : (
        <div class="grid gap-2.5">
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
  onCopyBaseUrl,
}: {
  provider: Provider;
  user: AdminUser;
  onBack: () => void;
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
  const canShare = target === null && user.role === "owner";
  const [name, setName] = useState(target?.name ?? "");
  const [enabled, setEnabled] = useState(target?.enabled ?? true);
  const [secret, setSecret] = useState("");
  const [shared, setShared] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const effectiveScheme = shared ? "none" : "bearer";
  const showCredential = effectiveScheme !== "none";

  const submit = async () => {
    setError(null);
    setBusy(true);
    try {
      const input = { name, enabled, authScheme: effectiveScheme, secret };
      if (target) {
        await api(`/admin/api/providers/${encodeURIComponent(provider.id)}/keys/${encodeURIComponent(target.id)}`, {
          method: "PUT",
          body: keyUpdatePayload(input),
        });
      } else {
        await api(`/admin/api/providers/${encodeURIComponent(provider.id)}/keys`, {
          method: "POST",
          body: keyCreatePayload(input, { shared }),
        });
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
          <label class="block text-xs font-semibold">
            API key
            <input class="input mt-1" type="password" autoComplete="off" value={secret} disabled={busy}
              placeholder={target?.has_secret ? "leave blank to keep the stored key" : "paste the provider API key"}
              onInput={(e) => setSecret((e.currentTarget as HTMLInputElement).value)} />
            <span class="mt-1 block text-[11px] font-normal text-dim">
              Write-only. The key is encrypted at rest; only the last four characters are shown.
            </span>
          </label>
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
}: {
  endpoint: string;
  provider: Provider;
  initial: KeyModel[];
  onSaved: (models: KeyModel[]) => void;
}) {
  const [rows, setRows] = useState<KeyModel[]>(initial);
  const [alias, setAlias] = useState("");
  const [upstream, setUpstream] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const choices = modelChoices(provider.models, rows);

  const add = (value: string, upstreamModel: string) => {
    if (!value.trim()) return;
    setRows(toggleKeyModel(rows, { alias: value, upstream_model: buildUpstreamModel(value, upstreamModel) }, true));
    setAlias("");
    setUpstream("");
  };

  const update = (target: string, next: Partial<KeyModel>) => {
    setRows(rows.map((item) => (item.alias === target ? { ...item, ...next } : item)));
  };

  const save = async () => {
    setBusy(true);
    setError(null);
    try {
      const next = parseKeyModels(await api<unknown>(endpoint, { method: "PUT", body: keyModelsPayload(rows) }));
      setRows(next);
      onSaved(next);
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="space-y-2 py-1">
      <div class="flex items-center gap-2">
        <b class="text-xs">Models for this key</b>
        <span class="text-[11px] text-dim">{rows.length} selected</span>
        <button class="btn btn-sm ml-auto" disabled={busy} onClick={() => void save()}>
          {busy ? "Saving…" : "Save models"}
        </button>
      </div>
      {rows.map((item) => (
        <div key={item.alias} class="flex flex-wrap items-center gap-2">
          <code class="mono w-[180px] truncate text-[11px]" title={item.alias}>{item.alias}</code>
          <input
            class="input mono h-7 w-[220px]"
            aria-label={`Upstream model for ${item.alias}`}
            value={item.upstream_model}
            placeholder={item.alias.endsWith("/*") ? "(suffix)" : "upstream model"}
            onInput={(e) => update(item.alias, { upstream_model: (e.currentTarget as HTMLInputElement).value })}
          />
          <label class="flex items-center gap-1.5 text-[11px] text-dim">
            <Switch checked={item.hidden} onChange={(v) => update(item.alias, { hidden: v })} label={`Hide ${item.alias}`} />
            hidden
          </label>
          <button class="btn btn-sm" aria-label={`Remove ${item.alias}`} onClick={() => setRows(rows.filter((row) => row.alias !== item.alias))}>
            <IconX />
          </button>
        </div>
      ))}
      {rows.length === 0 && <div class="text-[11px] text-dim">No models selected for this key.</div>}
      <div class="flex flex-wrap items-center gap-2 border-t border-line pt-2">
        <input class="input mono h-7 w-[180px]" aria-label="New model alias" placeholder="alias or vendor/*" value={alias}
          onInput={(e) => setAlias((e.currentTarget as HTMLInputElement).value)} />
        <input class="input mono h-7 w-[220px]" aria-label="New upstream model" placeholder="upstream model (defaults to alias)" value={upstream}
          onInput={(e) => setUpstream((e.currentTarget as HTMLInputElement).value)} />
        <button class="btn btn-sm" disabled={!alias.trim()} onClick={() => add(alias, upstream)}>Add model</button>
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

function MenuRow({ label, onClick, danger }: { label: string; onClick: () => void; danger?: boolean }) {
  return (
    <button class={`block w-full rounded-md px-2.5 py-1.5 text-left text-xs ${danger ? "text-bad" : "text-fg"} hover:bg-panel2`} onClick={onClick}>
      {label}
    </button>
  );
}
