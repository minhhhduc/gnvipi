import type { ComponentChildren } from "preact";
import { useMemo, useState } from "preact/hooks";
import { api, ApiError } from "../lib/api";
import { nsToSeconds, secondsToNs } from "../lib/format";
import { buildUpstreamModel, getProtocolOptions } from "../lib/helpers";
import type { Provider, TestResult } from "../lib/types";
import { Dialog, IconPlus, IconX, Switch, useUi } from "./ui";

interface MappingRow {
  alias: string;
  upstream: string;
  hidden: boolean;
  fallbacks: string;
  statuses: string;
  allowShadow: boolean;
  legacy: string;
  expanded: boolean;
}

interface HeaderRow {
  name: string;
  value: string;
}

function num(value: number | undefined): string {
  return value ? String(value) : "";
}

function json(value: unknown): string {
  if (!value || (typeof value === "object" && Object.keys(value).length === 0)) return "";
  return JSON.stringify(value, null, 2);
}

function parseJson(value: string, field: string): { value?: Record<string, unknown>; error?: string } {
  if (!value.trim()) return {};
  try {
    const parsed = JSON.parse(value);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
      return { error: `${field} must be a JSON object` };
    }
    return { value: parsed as Record<string, unknown> };
  } catch {
    return { error: `${field} is not valid JSON` };
  }
}

export function formatProviderTestResult(result: TestResult): string {
  const detail = result.message || result.error;
  return [
    result.latency_ms != null ? `${result.latency_ms}ms` : null,
    result.status,
    result.status_code != null ? `HTTP ${result.status_code}` : null,
    detail || null,
  ].filter(Boolean).join(" · ");
}

export function ProviderTestResultBadge({
  result,
  className = "",
}: {
  result: TestResult;
  className?: string;
}) {
  return (
    <div class={`flex min-w-0 flex-wrap items-center gap-1.5 ${className}`}>
      <span class={`pill ${result.ok ? "pill-ok" : "pill-bad"}`}>{result.ok ? "OK" : "FAIL"}</span>
      <span class={`min-w-0 max-w-full break-words text-[11px] ${result.ok ? "text-dim" : "text-bad"}`}>
        last test in this session · {formatProviderTestResult(result)}
      </span>
    </div>
  );
}

function init(provider: Provider | null) {
  return {
    id: provider?.id || "",
    name: provider?.name || "",
    base_url: provider?.base_url || "",
    proxy_url: provider?.proxy_url || "",
    protocol: provider?.protocol || "openai-chat",
    enabled: provider?.enabled ?? true,
    tls: provider?.tls || "default",
    scheme: provider?.auth?.scheme === "api-key" ? "x-api-key" : provider?.auth?.scheme || "bearer",
    key: "",
    keyEnv: provider?.auth?.key_env || "",
    keyRef: provider?.auth?.key_ref || "",
    headerName: provider?.auth?.header_name || "",
    headers: Object.entries(provider?.headers || {}).map(([name, value]) => ({ name, value })) as HeaderRow[],
    mappings: (provider?.models || []).map((m) => ({
      alias: m.alias,
      upstream: m.upstream_model || "",
      hidden: !!m.hidden,
      fallbacks: (m.fallbacks || []).join(", "),
      statuses: (m.fallback_statuses || []).join(", "),
      allowShadow: !!m.allow_shadow,
      legacy: num(m.legacy_position),
      expanded: false,
    })) as MappingRow[],
    forwardHeaders: (provider?.forward_headers || []).join(", "),
    endpointPath: json(provider?.endpoint_path),
    extraBody: json(provider?.extra_body),
    extensions: json(provider?.extensions),
    connect: nsToSeconds(provider?.timeouts?.connect),
    tlsTimeout: nsToSeconds(provider?.timeouts?.tls),
    responseHeader: nsToSeconds(provider?.timeouts?.response_header),
    idle: nsToSeconds(provider?.timeouts?.idle),
    total: nsToSeconds(provider?.timeouts?.total),
    maxConcurrency: num(provider?.max_concurrency),
    queueTimeout: nsToSeconds(provider?.queue_timeout),
  };
}

export function ProviderEditor({
  provider,
  onClose,
  onSaved,
}: {
  provider: Provider | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { toast, setBanner } = useUi();
  const isEdit = !!provider;
  const [f, setF] = useState(() => init(provider));
  const [dirty, setDirty] = useState(false);
  const [showClose, setShowClose] = useState(false);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [test, setTest] = useState<TestResult | null>(null);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [formError, setFormError] = useState<string | null>(null);

  const patch = (next: Partial<typeof f>) => {
    setF((prev) => ({ ...prev, ...next }));
    setDirty(true);
  };

  const close = () => {
    if (dirty) setShowClose(true);
    else onClose();
  };

  const buildPayload = () => {
    const errs: Record<string, string> = {};
    const extra = parseJson(f.extraBody, "Extra body");
    const extensions = parseJson(f.extensions, "Extensions");
    const endpointPath = parseJson(f.endpointPath, "Endpoint paths");
    if (extra.error) errs.extraBody = extra.error;
    if (extensions.error) errs.extensions = extensions.error;
    if (endpointPath.error) errs.endpointPath = endpointPath.error;
    if (!f.id.trim()) errs.id = "Provider ID is required";
    if (!f.name.trim()) errs.name = "Name is required";
    if (!f.base_url.trim()) errs.base_url = "Base URL is required";
    setErrors(errs);
    if (Object.keys(errs).length) return null;

    const auth: Provider["auth"] = { scheme: f.scheme };
    if (f.scheme !== "none") {
      if (f.key.trim()) auth.key = f.key.trim();
      if (f.keyEnv.trim()) auth.key_env = f.keyEnv.trim();
      if (f.keyRef.trim()) auth.key_ref = f.keyRef.trim();
      if (f.headerName.trim()) auth.header_name = f.headerName.trim();
    }
    const mapping = f.mappings
      .filter((m) => m.alias.trim())
      .map((m) => {
        const alias = m.alias.trim();
        const item: NonNullable<Provider["models"]>[number] = {
          alias,
          upstream_model: buildUpstreamModel(alias, m.upstream),
        };
        if (m.hidden) item.hidden = true;
        const fallbacks = m.fallbacks.split(",").map((s) => s.trim()).filter(Boolean);
        const statuses = m.statuses
          .split(",")
          .map((s) => parseInt(s.trim(), 10))
          .filter((n) => Number.isFinite(n));
        if (fallbacks.length) item.fallbacks = fallbacks;
        if (statuses.length) item.fallback_statuses = statuses;
        if (m.allowShadow) item.allow_shadow = true;
        const legacy = parseInt(m.legacy, 10);
        if (Number.isFinite(legacy)) item.legacy_position = legacy;
        return item;
      });
    const headers: Record<string, string> = {};
    f.headers.forEach((h) => {
      if (h.name.trim()) headers[h.name.trim()] = h.value;
    });
    const timeouts: NonNullable<Provider["timeouts"]> = {};
    [
      ["connect", f.connect],
      ["tls", f.tlsTimeout],
      ["response_header", f.responseHeader],
      ["idle", f.idle],
      ["total", f.total],
    ].forEach(([key, value]) => {
      const ns = secondsToNs(String(value));
      if (ns > 0) (timeouts as Record<string, number>)[key as string] = ns;
    });

    const payload: Provider = {
      ...(provider || {}),
      id: f.id.trim(),
      name: f.name.trim(),
      base_url: f.base_url.trim(),
      protocol: f.protocol,
      enabled: f.enabled,
      tls: f.tls,
      auth,
      models: mapping,
      headers,
      forward_headers: f.forwardHeaders.split(",").map((s) => s.trim()).filter(Boolean),
      endpoint_path: (endpointPath.value || {}) as Record<string, string>,
      extra_body: extra.value || {},
      extensions: extensions.value || {},
      max_concurrency: parseInt(f.maxConcurrency, 10) || 0,
      queue_timeout: secondsToNs(f.queueTimeout),
      timeouts,
    };
    if (!f.proxy_url.trim()) delete payload.proxy_url;
    else payload.proxy_url = f.proxy_url.trim();
    return payload;
  };

  const applyApiError = (err: unknown) => {
    const message = err instanceof Error ? err.message : "Request failed";
    setFormError(message);
    if (err instanceof ApiError && err.body) {
      const fieldErrors: Record<string, string> = {};
      const body = err.body;
      Object.entries(body.fields || {}).forEach(([k, v]) => {
        fieldErrors[k] = v;
      });
      (body.field_errors || []).forEach((fe) => {
        fieldErrors[fe.field] = fe.message;
      });
      if (body.field && body.error) fieldErrors[body.field] = body.error;
      setErrors((prev) => ({ ...prev, ...fieldErrors }));
    }
  };

  const save = async () => {
    const payload = buildPayload();
    if (!payload) return;
    setSaving(true);
    setFormError(null);
    try {
      await api(isEdit ? `/admin/api/providers/${encodeURIComponent(payload.id)}` : "/admin/api/providers", {
        method: isEdit ? "PUT" : "POST",
        body: payload,
      });
      toast(`${isEdit ? "Updated" : "Created"} ${payload.id}`);
      setDirty(false);
      onSaved();
    } catch (err) {
      applyApiError(err);
      setBanner(`Provider save failed: ${err instanceof Error ? err.message : "unknown error"}`, () => void save());
    } finally {
      setSaving(false);
    }
  };

  const runTest = async () => {
    const payload = buildPayload();
    if (!payload) return;
    setTesting(true);
    try {
      const result = await api<TestResult>(
        isEdit ? `/admin/api/providers/${encodeURIComponent(payload.id)}/test` : "/admin/api/providers/test",
        { method: "POST", body: payload },
      );
      setTest(result);
      setBanner(
        result.ok ? null : `Provider test failed: ${payload.name}: ${formatProviderTestResult(result)}`,
        result.ok ? undefined : () => void runTest(),
        "provider-test",
      );
    } catch (err) {
      const result: TestResult = { ok: false, status: "error", error: err instanceof Error ? err.message : "Test failed" };
      setTest(result);
      setBanner(`Provider test failed: ${payload.name}: ${formatProviderTestResult(result)}`, () => void runTest(), "provider-test");
    } finally {
      setTesting(false);
    }
  };

  const setMapping = (i: number, next: Partial<MappingRow>) => {
    const mappings = f.mappings.map((m, idx) => (idx === i ? { ...m, ...next } : m));
    patch({ mappings });
  };

  const mappingSummary = useMemo(() => f.mappings.length, [f.mappings.length]);
  const fieldError = (key: string) => errors[key] && <div class="mt-1 text-[11px] text-bad">{errors[key]}</div>;

  return (
    <>
      <div class="fixed inset-0 z-30 bg-black/45" onClick={close} />
      <aside
        class="fixed inset-y-0 right-0 z-30 flex w-[560px] max-w-[92vw] flex-col border-l border-line bg-panel shadow-xl"
        role="dialog"
        aria-modal="true"
        aria-label={isEdit ? `Edit provider ${provider?.id}` : "Add provider"}
      >
        <div class="flex items-center gap-2.5 border-b border-line px-[18px] py-[15px]">
          <b class="text-[15px]">{isEdit ? `Edit provider · ${provider?.id}` : "Add provider"}</b>
          {dirty && <span class="pill pill-mute ml-auto">unsaved changes</span>}
          <button class="btn btn-sm ml-auto" onClick={close} aria-label="Close">
            <IconX />
          </button>
        </div>

        {formError && <div class="border-b border-bad bg-bad-soft px-[18px] py-2 text-xs text-bad">{formError}</div>}

        <div class="flex-1 overflow-auto px-[18px] py-4">
          <Section title="Identity">
            <Field label="Provider ID">
              <input class="input mono" value={f.id} readOnly={isEdit} onInput={(e) => patch({ id: (e.target as HTMLInputElement).value })} />
              {isEdit && <div class="mt-1 text-[11px] text-dim">Provider IDs are immutable.</div>}
              {fieldError("id")}
            </Field>
            <Field label="Name">
              <input class="input" value={f.name} onInput={(e) => patch({ name: (e.target as HTMLInputElement).value })} />
              {fieldError("name")}
            </Field>
            <Field label="Base URL">
              <input class="input mono" value={f.base_url} onInput={(e) => patch({ base_url: (e.target as HTMLInputElement).value })} />
              {fieldError("base_url")}
            </Field>
            <Field label="Proxy URL">
              <input class="input mono" value={f.proxy_url} placeholder="(empty uses environment proxy)" onInput={(e) => patch({ proxy_url: (e.target as HTMLInputElement).value })} />
            </Field>
            <div class="grid grid-cols-2 gap-3">
              <Field label="Protocol">
                <select class="input" value={f.protocol} onChange={(e) => patch({ protocol: (e.target as HTMLSelectElement).value })}>
                  {getProtocolOptions(f.protocol, provider?.protocol).map((p) => (
                    <option key={p} value={p}>{p}</option>
                  ))}
                </select>
              </Field>
              <Field label="TLS profile">
                <input class="input" value={f.tls} onInput={(e) => patch({ tls: (e.target as HTMLInputElement).value })} />
              </Field>
            </div>
            <label class="mt-2 flex items-center gap-2 text-xs font-semibold">
              <Switch checked={f.enabled} onChange={(v) => patch({ enabled: v })} label="Provider enabled" />
              Enabled
            </label>
          </Section>

          <Section title="Authentication">
            <Field label="Auth scheme">
              <select class="input" value={f.scheme} onChange={(e) => patch({ scheme: (e.target as HTMLSelectElement).value })}>
                <option value="bearer">Bearer</option>
                <option value="x-api-key">x-api-key</option>
                <option value="none">None</option>
              </select>
            </Field>
            <Field label="API key">
              <input class="input" type="password" value={f.key} placeholder={isEdit ? "leave blank to keep existing key" : "write-only"} onInput={(e) => patch({ key: (e.target as HTMLInputElement).value })} />
            </Field>
            <Field label="API key env var">
              <input class="input mono" value={f.keyEnv} onInput={(e) => patch({ keyEnv: (e.target as HTMLInputElement).value })} />
            </Field>
            <Field label="Key ref">
              <input class="input mono" value={f.keyRef} onInput={(e) => patch({ keyRef: (e.target as HTMLInputElement).value })} />
            </Field>
            <Field label="Header name">
              <input class="input mono" value={f.headerName} onInput={(e) => patch({ headerName: (e.target as HTMLInputElement).value })} />
            </Field>
          </Section>

          <Section title="Model mappings">
            {f.mappings.map((m, i) => (
              <div key={i} class="mb-2 rounded-lg border border-line p-2.5">
                <div class="flex items-center gap-2">
                  <input class="input mono" placeholder="alias" value={m.alias} onInput={(e) => setMapping(i, { alias: (e.target as HTMLInputElement).value })} />
                  <input class="input mono" placeholder="upstream model" value={m.upstream} onInput={(e) => setMapping(i, { upstream: (e.target as HTMLInputElement).value })} />
                  <Switch checked={m.hidden} onChange={(v) => setMapping(i, { hidden: v })} label={`Hide ${m.alias || "mapping"}`} />
                  <button class="btn btn-sm" onClick={() => setMapping(i, { expanded: !m.expanded })}>
                    {m.expanded ? "Less" : "More"}
                  </button>
                  <button class="btn btn-sm" aria-label="Remove mapping" onClick={() => patch({ mappings: f.mappings.filter((_, idx) => idx !== i) })}>
                    <IconX />
                  </button>
                </div>
                {m.expanded && (
                  <div class="mt-2 grid grid-cols-2 gap-2">
                    <Field label="Fallbacks (comma-separated)">
                      <input class="input mono" value={m.fallbacks} onInput={(e) => setMapping(i, { fallbacks: (e.target as HTMLInputElement).value })} />
                    </Field>
                    <Field label="Fallback statuses (comma-separated)">
                      <input class="input mono" value={m.statuses} placeholder="429, 500" onInput={(e) => setMapping(i, { statuses: (e.target as HTMLInputElement).value })} />
                    </Field>
                    <Field label="Legacy position">
                      <input class="input" type="number" value={m.legacy} onInput={(e) => setMapping(i, { legacy: (e.target as HTMLInputElement).value })} />
                    </Field>
                    <label class="flex items-center gap-2 pt-5 text-xs font-semibold">
                      <Switch checked={m.allowShadow} onChange={(v) => setMapping(i, { allowShadow: v })} label={`Allow shadow ${m.alias || "mapping"}`} />
                      Allow shadow
                    </label>
                  </div>
                )}
              </div>
            ))}
            <button
              class="btn btn-sm"
              onClick={() =>
                patch({
                  mappings: [...f.mappings, { alias: "", upstream: "", hidden: false, fallbacks: "", statuses: "", allowShadow: false, legacy: "", expanded: true }],
                })
              }
            >
              <IconPlus /> Add mapping
            </button>
            <div class="mt-1 text-[11px] text-dim">{mappingSummary} mapping{mappingSummary === 1 ? "" : "s"}</div>
          </Section>

          <Section title="Headers & routing">
            {f.headers.map((h, i) => (
              <div class="mb-2 flex items-center gap-2">
                <input class="input mono" placeholder="Header name" value={h.name} onInput={(e) => patch({ headers: f.headers.map((x, idx) => (idx === i ? { ...x, name: (e.target as HTMLInputElement).value } : x)) })} />
                <input class="input mono" placeholder="Value" value={h.value} onInput={(e) => patch({ headers: f.headers.map((x, idx) => (idx === i ? { ...x, value: (e.target as HTMLInputElement).value } : x)) })} />
                <button class="btn btn-sm" aria-label="Remove header" onClick={() => patch({ headers: f.headers.filter((_, idx) => idx !== i) })}>
                  <IconX />
                </button>
              </div>
            ))}
            <button class="btn btn-sm" onClick={() => patch({ headers: [...f.headers, { name: "", value: "" }] })}>
              <IconPlus /> Add header
            </button>
            <Field label="Forward headers (comma-separated)">
              <input class="input mono" value={f.forwardHeaders} onInput={(e) => patch({ forwardHeaders: (e.target as HTMLInputElement).value })} />
            </Field>
            <Field label="Endpoint path overrides (JSON object)">
              <textarea class="input mono" rows={2} value={f.endpointPath} onInput={(e) => patch({ endpointPath: (e.target as HTMLTextAreaElement).value })} />
              {fieldError("endpointPath")}
            </Field>
          </Section>

          <Section title="Advanced">
            <div class="grid grid-cols-3 gap-2">
              <Field label="Connect (s)">
                <input class="input" value={f.connect} onInput={(e) => patch({ connect: (e.target as HTMLInputElement).value })} />
              </Field>
              <Field label="TLS (s)">
                <input class="input" value={f.tlsTimeout} onInput={(e) => patch({ tlsTimeout: (e.target as HTMLInputElement).value })} />
              </Field>
              <Field label="Response header (s)">
                <input class="input" value={f.responseHeader} onInput={(e) => patch({ responseHeader: (e.target as HTMLInputElement).value })} />
              </Field>
              <Field label="Idle (s)">
                <input class="input" value={f.idle} onInput={(e) => patch({ idle: (e.target as HTMLInputElement).value })} />
              </Field>
              <Field label="Total (s)">
                <input class="input" value={f.total} onInput={(e) => patch({ total: (e.target as HTMLInputElement).value })} />
              </Field>
              <Field label="Max concurrency">
                <input class="input" value={f.maxConcurrency} onInput={(e) => patch({ maxConcurrency: (e.target as HTMLInputElement).value })} />
              </Field>
              <Field label="Queue timeout (s)">
                <input class="input" value={f.queueTimeout} onInput={(e) => patch({ queueTimeout: (e.target as HTMLInputElement).value })} />
              </Field>
            </div>
            <Field label="Extra body (JSON object)">
              <textarea class="input mono" rows={3} value={f.extraBody} onInput={(e) => patch({ extraBody: (e.target as HTMLTextAreaElement).value })} />
              {fieldError("extraBody")}
            </Field>
            <Field label="Extensions (JSON object)">
              <textarea class="input mono" rows={3} value={f.extensions} onInput={(e) => patch({ extensions: (e.target as HTMLTextAreaElement).value })} />
              {fieldError("extensions")}
            </Field>
          </Section>
        </div>

        <div class="flex flex-wrap items-center gap-2 border-t border-line bg-panel2 px-[18px] py-3">
          {test && <ProviderTestResultBadge result={test} className="w-full" />}
          <span class="ml-auto" />
          <button class="btn" onClick={runTest} disabled={testing || saving}>
            {testing ? "Testing…" : "Test connection"}
          </button>
          <button class="btn" onClick={close} disabled={saving}>
            Cancel
          </button>
          <button class="btn btn-primary" onClick={save} disabled={saving}>
            {saving ? "Saving…" : "Save provider"}
          </button>
        </div>
      </aside>
      {showClose && (
        <Dialog
          title="Discard unsaved changes?"
          confirmLabel="Discard"
          danger
          onCancel={() => setShowClose(false)}
          onConfirm={() => {
            setShowClose(false);
            onClose();
          }}
        >
          The provider editor has unsaved changes. Closing now discards them.
        </Dialog>
      )}
    </>
  );
}

function Section({ title, children }: { title: string; children: ComponentChildren }) {
  return (
    <section class="mb-3 border-t border-line pt-3 first:border-t-0 first:pt-0">
      <h3 class="mb-2 text-xs font-semibold uppercase tracking-[0.05em] text-dim">{title}</h3>
      {children}
    </section>
  );
}

function Field({ label, children }: { label: string; children: ComponentChildren }) {
  return (
    <label class="mb-2.5 block text-xs font-semibold">
      <span class="mb-1 block">{label}</span>
      {children}
    </label>
  );
}
