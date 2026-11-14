import { useEffect, useRef, useState } from "preact/hooks";
import { api } from "../lib/api";
import type { AdminUser } from "../lib/auth";
import type { AdminUserRecord } from "../lib/types";
import { message } from "../lib/admin";
import { USER_LIST_MAX, parseUserList } from "../lib/users";
import {
  groupModels,
  isStoppedError,
  parseModels,
  streamPlaygroundChat,
  type PlaygroundModel,
} from "../lib/playground";
import { Card, CardHead, Empty, PageHeader } from "../components/ui";

export function PlaygroundView({ user }: { user: AdminUser }) {
  const owner = user.role === "owner";
  const [accounts, setAccounts] = useState<AdminUserRecord[]>([]);
  const [accountID, setAccountID] = useState("");
  const [models, setModels] = useState<PlaygroundModel[]>([]);
  const [union, setUnion] = useState(false);
  const [model, setModel] = useState("");
  const [modelsLoading, setModelsLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [input, setInput] = useState("");
  const [output, setOutput] = useState("");
  const [error, setError] = useState("");
  const [streaming, setStreaming] = useState(false);
  const abort = useRef<AbortController | null>(null);

  useEffect(() => {
    if (!owner) return;
    api<unknown>(`/admin/api/users?limit=${USER_LIST_MAX}`)
      .then((body) => setAccounts(parseUserList(body).users))
      // The playground still works for the caller's own account; the selector
      // just stays on "My account".
      .catch(() => setAccounts([]));
  }, [owner]);

  useEffect(() => {
    let cancelled = false;
    setModelsLoading(true);
    const query = owner && accountID ? `?user_id=${encodeURIComponent(accountID)}` : "";
    api<unknown>(`/admin/api/playground/models${query}`)
      .then((body) => {
        if (cancelled) return;
        const parsed = parseModels(body);
        setModels(parsed.models);
        setUnion(parsed.union);
        setLoadError("");
      })
      .catch((err) => {
        if (cancelled) return;
        setModels([]);
        setLoadError(message(err));
      })
      .finally(() => {
        if (!cancelled) setModelsLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [owner, accountID]);

  useEffect(() => {
    setModel((current) => (models.some((entry) => entry.alias === current) ? current : models[0]?.alias ?? ""));
  }, [models]);

  // A sign-out or account switch must not leave an old stream rendering.
  useEffect(() => () => abort.current?.abort(), []);

  const send = async () => {
    const prompt = input.trim();
    if (!prompt || !model || streaming) return;
    const controller = new AbortController();
    abort.current = controller;
    setStreaming(true);
    setOutput("");
    setError("");
    try {
      await streamPlaygroundChat({
        model,
        messages: [{ role: "user", content: prompt }],
        userId: owner && accountID ? accountID : undefined,
        signal: controller.signal,
        onDelta: (text) => setOutput((current) => current + text),
      });
    } catch (err) {
      if (!isStoppedError(err)) setError(message(err));
    } finally {
      abort.current = null;
      setStreaming(false);
    }
  };

  const stop = () => abort.current?.abort();

  const groups = groupModels(models);
  const selected = models.find((entry) => entry.alias === model);

  return (
    <>
      <PageHeader title="Playground" sub="Send a test message through this gateway as the signed-in account">
        {owner && (
          <label class="flex items-center gap-2 text-xs text-dim">
            Test as
            <select class="input" value={accountID} onChange={(e) => setAccountID((e.target as HTMLSelectElement).value)}>
              <option value="">My account ({user.username})</option>
              {accounts
                .filter((account) => account.id !== user.id)
                .map((account) => (
                  <option key={account.id} value={account.id}>
                    {account.display_name || account.username} ({account.role})
                  </option>
                ))}
            </select>
          </label>
        )}
      </PageHeader>

      <Card>
        <CardHead title="Message" sub="Only models this account can resolve are listed">
          {selected && (
            <span class="text-xs text-dim">
              {selected.provider} · <code>{selected.upstream_model}</code>
            </span>
          )}
        </CardHead>

        {loadError && (
          <div role="alert" class="mb-3 rounded-lg border border-bad bg-bad-soft px-3 py-2 text-xs text-bad">
            Models unavailable: {loadError}
          </div>
        )}

        {modelsLoading ? (
          <p class="text-sm text-dim">Loading models…</p>
        ) : models.length === 0 ? (
          <Empty
            title="No usable models"
            body="Add a provider key on the Models page, or start the NVIDIA Chrome pool to use the shared Playground catalog."
          />
        ) : (
          <div class="grid gap-3">
            <label class="text-xs font-semibold text-dim" for="playground-model">Model</label>
            <select id="playground-model" class="input" value={model} onChange={(e) => setModel((e.target as HTMLSelectElement).value)}>
              {groups.map((group) => (
                <optgroup key={group.provider} label={group.provider}>
                  {group.models.map((entry) => (
                    <option key={`${entry.provider}:${entry.alias}:${entry.upstream_model}`} value={entry.alias}>
                      {entry.alias === entry.upstream_model ? entry.alias : `${entry.alias} → ${entry.upstream_model}`}
                    </option>
                  ))}
                </optgroup>
              ))}
            </select>
            {union && (
              <p class="text-[11px] text-dim">
                Showing the union of every account's models. Pick an account above to test as that account.
              </p>
            )}
            <label class="text-xs font-semibold text-dim" for="playground-message">Message</label>
            <textarea
              id="playground-message"
              class="input min-h-[96px] resize-y font-mono text-[13px]"
              rows={4}
              placeholder="Write a test message…"
              value={input}
              disabled={streaming}
              onInput={(e) => setInput((e.target as HTMLTextAreaElement).value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !e.shiftKey) {
                  e.preventDefault();
                  void send();
                }
              }}
            />
            <div class="flex items-center gap-2">
              <button type="button" class="btn btn-primary" disabled={streaming || !model || !input.trim()} onClick={() => void send()}>
                Send
              </button>
              <button type="button" class="btn" disabled={!streaming} onClick={stop}>Stop</button>
              {streaming && <span class="text-xs text-dim" role="status">Streaming…</span>}
            </div>
          </div>
        )}
      </Card>

      {error && (
        <div role="alert" class="mt-3 rounded-[10px] border border-bad bg-bad-soft px-[13px] py-2.5 text-xs text-bad">
          <b>Request failed:</b> {error}
        </div>
      )}

      <Card class="mt-3">
        <CardHead title="Reply" sub={selected ? `as ${accountID ? accounts.find((a) => a.id === accountID)?.username ?? "account" : user.username}` : "No reply yet"} />
        <pre class="whitespace-pre-wrap break-words font-mono text-[13px] leading-6 text-fg">
          {output || (streaming ? "…" : "Send a message to see the streamed reply.")}
        </pre>
      </Card>
    </>
  );
}
