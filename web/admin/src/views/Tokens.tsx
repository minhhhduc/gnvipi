import { useEffect, useRef, useState } from "preact/hooks";
import { api } from "../lib/api";
import { useUi } from "../components/ui";
import { Card, CardHead, Dialog, Empty, PageHeader, Pager } from "../components/ui";
import { isSuperuserLevelRole, type AdminUser } from "../lib/auth";
import type { AdminUserRecord, TokenRecord } from "../lib/types";
import {
  defaultExpiryLabel,
  emptyPageOffset,
  expiryChoiceAllowed,
  expiryLabel,
  lastUsedLabel,
  parseCreatedToken,
  parseTokenList,
  tokenCreatePayload,
  tokenStatus,
  type ExpiryChoice,
  type TokenStatus,
} from "../lib/tokens";
import { USER_LIST_MAX, parseUserList, userPickerNote } from "../lib/users";
import {
  DEFAULT_LIMIT,
  PAGE_SIZES,
  clampLimit,
  dateLabel,
  message,
  pageWindow,
} from "../lib/admin";

type DialogState =
  | { kind: "create" }
  | { kind: "created"; token: string; name: string }
  | { kind: "revoke"; token: TokenRecord }
  | null;

function statusPill(status: TokenStatus) {
  if (status === "active") return "pill pill-ok";
  return status === "expired" ? "pill pill-warn" : "pill pill-bad";
}

export function TokensView({ user }: { user: AdminUser }) {
  const { toast, setBanner } = useUi();
  const superuser = isSuperuserLevelRole(user.role);
  const [tokens, setTokens] = useState<TokenRecord[]>([]);
  const [limit, setLimit] = useState(DEFAULT_LIMIT);
  const [offset, setOffset] = useState(0);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [dialog, setDialog] = useState<DialogState>(null);
  const [reveal, setReveal] = useState<{
    token: TokenRecord;
    code: string;
    secret: string | null;
    busy: boolean;
    error: string | null;
  } | null>(null);
  // Superusers may read another account's tokens one account at a time; every
  // other role is pinned to its own tokens by the server.
  const [accounts, setAccounts] = useState<AdminUserRecord[]>([]);
  const [accountID, setAccountID] = useState("");
  const requestId = useRef(0);

  const requestReveal = async (token: TokenRecord) => {
    setReveal({ token, code: "", secret: null, busy: true, error: null });
    try {
      await api(`/admin/api/tokens/${encodeURIComponent(token.id)}/reveal/request`, { method: "POST" });
      setReveal((r) => (r ? { ...r, busy: false } : r));
      toast("Verification code sent to email");
    } catch (err) {
      setReveal((r) => (r ? { ...r, busy: false, error: message(err) } : r));
    }
  };

  const verifyReveal = async () => {
    if (!reveal) return;
    setReveal((r) => (r ? { ...r, busy: true, error: null } : r));
    try {
      const res = await api<{ token?: string }>(`/admin/api/tokens/${encodeURIComponent(reveal.token.id)}/reveal`, {
        method: "POST",
        body: JSON.stringify({ otp: reveal.code.trim() }),
      });
      setReveal((r) => (r ? { ...r, busy: false, secret: res.token ?? "" } : r));
    } catch (err) {
      setReveal((r) => (r ? { ...r, busy: false, error: message(err) } : r));
    }
  };

  const load = async () => {
    const thisRequest = ++requestId.current;
    setLoading(true);
    setLoadError(null);
    const query = new URLSearchParams({ limit: String(limit), offset: String(offset) });
    if (superuser && accountID) query.set("user_id", accountID);
    try {
      const body = await api<unknown>(`/admin/api/tokens?${query.toString()}`);
      if (thisRequest !== requestId.current) return;
      const parsed = parseTokenList(body);
      if (parsed.tokens.length === 0 && offset > 0) {
        // The endpoint reports no total: an empty page proves the set shrank.
        setTokens([]);
        setOffset(emptyPageOffset(limit, offset));
        return;
      }
      setTokens(parsed.tokens);
      setBanner(null, undefined, "tokens");
    } catch (err) {
      if (thisRequest !== requestId.current) return;
      // Never leave another account's rows rendered (and revocable).
      setTokens([]);
      setLoadError(message(err));
      setBanner(`Tokens unavailable: ${message(err)}`, () => void load(), "tokens");
    } finally {
      if (thisRequest === requestId.current) setLoading(false);
    }
  };

  useEffect(() => {
    void load();
  }, [limit, offset, accountID]);

  useEffect(() => {
    if (!superuser) return;
    api<unknown>(`/admin/api/users?limit=${USER_LIST_MAX}`)
      .then((body) => setAccounts(parseUserList(body).users))
      // The token list still works for the caller's own account; the selector
      // just stays limited to "My tokens".
      .catch(() => setAccounts([]));
  }, [superuser]);

  const target = accountID ? accounts.find((account) => account.id === accountID) : undefined;
  const targetRole = target?.role ?? user.role;
  const pickerNote = userPickerNote(accounts.length);
  const revokeOwner = accountID ? (target?.username ?? "the selected account") : user.username;
  const window = pageWindow({ limit, offset, count: tokens.length });

  const changeLimit = (next: number) => {
    setLimit(clampLimit(next));
    setOffset(0);
  };

  return (
    <>
      <PageHeader
        title="API Keys"
        sub={superuser ? "Gateway and admin keys for every account" : "Your gateway API keys"}
      >
        <button class="btn btn-primary" onClick={() => setDialog({ kind: "create" })}>New API key</button>
      </PageHeader>

      <Card>
        <CardHead title={superuser ? "All API keys" : "My API keys"}>
          {superuser && (
            <>
              <label class="flex items-center gap-1.5 text-xs text-dim">
                Account
                <select
                  class="input w-auto"
                  value={accountID}
                  title={pickerNote ?? undefined}
                  onChange={(e) => {
                    // Drop the previous account's rows before the new list loads.
                    setTokens([]);
                    setOffset(0);
                    setAccountID((e.target as HTMLSelectElement).value);
                  }}
                >
                  <option value="">My tokens</option>
                  {accounts.filter((account) => account.id !== user.id).map((account) => (
                    <option key={account.id} value={account.id}>
                      {account.display_name || account.username} (@{account.username})
                    </option>
                  ))}
                </select>
              </label>
              {pickerNote && <span class="text-[11px] text-dim">{pickerNote}</span>}
            </>
          )}
          <label class="flex items-center gap-1.5 text-xs text-dim">
            Rows
            <select class="input w-auto" value={limit} onChange={(e) => changeLimit(Number((e.target as HTMLSelectElement).value))}>
              {PAGE_SIZES.map((size) => <option key={size} value={size}>{size}</option>)}
            </select>
          </label>
        </CardHead>
        {loadError ? (
          <Empty title="Tokens unavailable" body={loadError} />
        ) : loading && tokens.length === 0 ? (
          <Empty title="Loading…" body="Reading tokens." />
        ) : tokens.length === 0 ? (
          <Empty title="No tokens" body="Create a token with New token. The plaintext is shown once." />
        ) : (
          <table class="table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Kind</th>
                <th>Prefix</th>
                <th>Expires</th>
                <th>Last used</th>
                <th>Status</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {tokens.map((token) => {
                const status = tokenStatus(token);
                return (
                  <tr key={token.id}>
                    <td>
                      <div class="font-semibold">{token.name}</div>
                      <div class="text-[11px] text-dim">created {dateLabel(token.created_at)}</div>
                    </td>
                    <td><span class="chip">{token.kind}</span></td>
                    <td class="mono muted">{token.token_prefix}…</td>
                    <td class="muted">{expiryLabel(token.expires_at)}</td>
                    <td class="muted">{lastUsedLabel(token.last_used_at)}</td>
                    <td><span class={statusPill(status)}>{status}</span></td>
                    <td class="text-right whitespace-nowrap">
                      <div class="inline-flex items-center gap-2">
                        {token.can_reveal && status !== "revoked" && (
                          <button
                            class="btn btn-sm"
                            type="button"
                            title="Reveal token after OTP verification"
                            onClick={() => void requestReveal(token)}
                          >
                            Show
                          </button>
                        )}
                        <button
                          class="btn btn-sm btn-danger"
                          disabled={status === "revoked"}
                          title={status === "revoked" ? "Already revoked" : "Revoke this token"}
                          onClick={() => setDialog({ kind: "revoke", token })}
                        >
                          Revoke
                        </button>
                      </div>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
        <Pager
          from={window.from}
          to={window.to}
          hasPrev={window.hasPrev}
          hasNext={window.hasNext}
          onPrev={() => setOffset(Math.max(0, offset - limit))}
          onNext={() => setOffset(offset + limit)}
        />
      </Card>

      {dialog?.kind === "create" && (
        <CreateTokenDialog
          superuser={superuser}
          targetRole={targetRole}
          targetID={accountID}
          onClose={() => setDialog(null)}
          onCreated={(token, name) => {
            setDialog({ kind: "created", token, name });
            void load();
          }}
        />
      )}
      {dialog?.kind === "created" && (
        <CreatedTokenDialog token={dialog.token} name={dialog.name} onClose={() => setDialog(null)} />
      )}
      {dialog?.kind === "revoke" && (
        <RevokeTokenDialog
          token={dialog.token}
          owner={revokeOwner}
          onClose={() => setDialog(null)}
          onRevoked={() => { setDialog(null); toast("Token revoked"); void load(); }}
        />
      )}
      {reveal && (
        <Dialog
          title={`Reveal token · ${reveal.token.name}`}
          confirmLabel={reveal.secret !== null ? "Done" : reveal.busy ? "Verifying…" : "Verify & Show"}
          onConfirm={() => {
            if (reveal.secret !== null) setReveal(null);
            else void verifyReveal();
          }}
          onCancel={() => setReveal(null)}
          busy={reveal.busy}
        >
          {reveal.secret !== null ? (
            <div class="space-y-3">
              <textarea
                class="input mono h-20 select-all"
                readOnly
                value={reveal.secret}
                aria-label="Revealed API token"
              />
              <button
                class="btn"
                type="button"
                onClick={() => {
                  void navigator.clipboard.writeText(reveal.secret!).then(() => toast("Token copied")).catch(() => {});
                }}
              >
                Copy token
              </button>
            </div>
          ) : (
            <div class="space-y-3">
              <p class="m-0 text-sm text-dim">
                Enter the 6-digit verification code sent to the token owner&apos;s email to reveal{" "}
                <span class="mono">{reveal.token.token_prefix}…</span>.
              </p>
              <label class="block text-xs font-semibold text-dim">
                6-digit OTP
                <input
                  class="input mono mt-1"
                  type="text"
                  inputMode="numeric"
                  maxLength={6}
                  placeholder="123456"
                  value={reveal.code}
                  disabled={reveal.busy}
                  autoFocus
                  onInput={(e) => setReveal((r) => (r ? { ...r, code: (e.target as HTMLInputElement).value } : r))}
                />
              </label>
              {reveal.error && (
                <div role="alert" class="rounded-lg border border-bad bg-bad-soft px-3 py-2 text-xs text-bad">
                  {reveal.error}
                </div>
              )}
            </div>
          )}
        </Dialog>
      )}
    </>
  );
}

function CreateTokenDialog({
  superuser,
  targetRole,
  targetID,
  onClose,
  onCreated,
}: {
  superuser: boolean;
  targetRole: string;
  targetID: string;
  onClose: () => void;
  onCreated: (token: string, name: string) => void;
}) {
  const [name, setName] = useState("");
  const [kind, setKind] = useState("gateway");
  const [scope, setScope] = useState("read");
  const [expiry, setExpiry] = useState<ExpiryChoice>("default");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // User-role accounts only ever hold gateway tokens.
  const kinds = targetRole === "user" ? ["gateway"] : ["gateway", "admin"];
  const noExpiryAllowed = expiryChoiceAllowed(kind, "none");

  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      const payload = tokenCreatePayload({
        name,
        kind,
        scope: kind === "admin" ? scope : undefined,
        userId: superuser && targetID ? targetID : undefined,
        expiry,
      });
      const created = parseCreatedToken(await api("/admin/api/tokens", { method: "POST", body: payload }));
      onCreated(created.token, created.record.name);
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog title="New token" confirmLabel="Create" onConfirm={() => void submit()} onCancel={onClose} busy={busy}>
      <div class="space-y-3">
        <label class="block text-xs font-semibold">
          Name
          <input class="input mt-1" value={name} disabled={busy} autoFocus placeholder="laptop, ci, cli…"
            onInput={(e) => setName((e.currentTarget as HTMLInputElement).value)} />
        </label>
        <label class="block text-xs font-semibold">
          Kind
          <select class="input mt-1" value={kind} disabled={busy}
            onChange={(e) => {
              const next = (e.target as HTMLSelectElement).value;
              setKind(next);
              if (!expiryChoiceAllowed(next, expiry)) setExpiry("default");
            }}>
            {kinds.map((value) => <option key={value} value={value}>{value}</option>)}
          </select>
          {targetRole === "user" && <span class="mt-1 block text-[11px] font-normal text-dim">User accounts hold gateway tokens only.</span>}
        </label>
        {kind === "admin" && (
          <label class="block text-xs font-semibold">
            Scope
            <select class="input mt-1" value={scope} disabled={busy}
              onChange={(e) => setScope((e.target as HTMLSelectElement).value)}>
              <option value="read">read</option>
              <option value="write">write</option>
            </select>
          </label>
        )}
        <label class="block text-xs font-semibold">
          Expiry
          <select class="input mt-1" value={expiry} disabled={busy}
            onChange={(e) => setExpiry((e.target as HTMLSelectElement).value as ExpiryChoice)}>
            <option value="default">{defaultExpiryLabel(kind)}</option>
            <option value="none" disabled={!noExpiryAllowed}>No expiry</option>
          </select>
          <span class="mt-1 block text-[11px] font-normal text-dim">
            {noExpiryAllowed
              ? "The default expires one year from creation; no-expiry tokens never do."
              : "Admin tokens always expire; the default is 90 days from creation."}
          </span>
        </label>
        {error && <div role="alert" class="rounded-lg border border-bad bg-bad-soft px-3 py-2 text-xs text-bad">{error}</div>}
      </div>
    </Dialog>
  );
}

function CreatedTokenDialog({ token, name, onClose }: { token: string; name: string; onClose: () => void }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(token);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  };
  return (
    <Dialog title={`Token created · ${name}`} confirmLabel="Done" onConfirm={onClose} onCancel={onClose} dismissible={false}>
      <div class="space-y-3">
        <p class="m-0">Copy this token now. It is stored hashed and cannot be shown again.</p>
        <textarea class="input mono h-20 select-all" readOnly value={token} aria-label="New token" />
        <button class="btn" type="button" onClick={() => void copy()}>{copied ? "Copied" : "Copy token"}</button>
      </div>
    </Dialog>
  );
}

function RevokeTokenDialog({
  token,
  owner,
  onClose,
  onRevoked,
}: {
  token: TokenRecord;
  owner: string;
  onClose: () => void;
  onRevoked: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const revoke = async () => {
    setBusy(true);
    setError(null);
    try {
      await api(`/admin/api/tokens/${token.id}`, { method: "DELETE" });
      onRevoked();
    } catch (err) {
      setError(message(err));
      setBusy(false);
    }
  };

  return (
    <Dialog title={`Revoke “${token.name}”?`} confirmLabel="Revoke token" danger onConfirm={() => void revoke()} onCancel={onClose} busy={busy}>
      <div class="space-y-2">
        <p class="m-0">
          This token belongs to <span class="mono">@{owner}</span>. Requests using{" "}
          <span class="mono">{token.token_prefix}…</span> will start failing immediately. This cannot be undone.
        </p>
        {error && <div role="alert" class="rounded-lg border border-bad bg-bad-soft px-3 py-2 text-xs text-bad">{error}</div>}
      </div>
    </Dialog>
  );
}