import { useEffect, useRef, useState } from "preact/hooks";
import { api } from "../lib/api";
import { useUi } from "../components/ui";
import { Card, CardHead, Dialog, Empty, PageHeader, Pager } from "../components/ui";
import { HealthPill } from "../components/Shell";
import type { AdminUser } from "../lib/auth";
import type { AdminUserRecord } from "../lib/types";
import {
  ROLES,
  canChangeRole,
  canManageUsers,
  displayNameError,
  parseUserList,
  passwordResetBody,
  roleError,
  validateNewPassword,
  validateNewUser,
  type FieldError,
} from "../lib/users";
import {
  DEFAULT_LIMIT,
  PAGE_SIZES,
  clampLimit,
  clampOffset,
  dateLabel,
  message,
  pageWindow,
  statusPill,
} from "../lib/admin";

type DialogState =
  | { kind: "create" }
  | { kind: "edit"; user: AdminUserRecord }
  | { kind: "password"; user: AdminUserRecord }
  | { kind: "delete"; user: AdminUserRecord }
  | null;

export function UsersView({ user }: { user: AdminUser }) {
  const { toast, setBanner } = useUi();
  const [users, setUsers] = useState<AdminUserRecord[]>([]);
  const [total, setTotal] = useState(0);
  const [limit, setLimit] = useState(DEFAULT_LIMIT);
  const [offset, setOffset] = useState(0);
  const [loading, setLoading] = useState(true);
  const [dialog, setDialog] = useState<DialogState>(null);
  const requestId = useRef(0);

  const load = async () => {
    const thisRequest = ++requestId.current;
    setLoading(true);
    try {
      const body = await api<unknown>(`/admin/api/users?limit=${limit}&offset=${offset}`);
      if (thisRequest !== requestId.current) return;
      const parsed = parseUserList(body);
      // Deleting the last row of a page shrinks the result set; move to the
      // last page that still has rows instead of showing an empty page.
      const nextOffset = clampOffset({ total: parsed.total, limit, offset });
      if (nextOffset !== offset) {
        setOffset(nextOffset);
        return;
      }
      setUsers(parsed.users);
      setTotal(parsed.total);
      setBanner(null, undefined, "users");
    } catch (err) {
      if (thisRequest !== requestId.current) return;
      setBanner(`Users unavailable: ${message(err)}`, () => void load(), "users");
    } finally {
      if (thisRequest === requestId.current) setLoading(false);
    }
  };

  useEffect(() => {
    void load();
  }, [limit, offset]);

  const changeLimit = (next: number) => {
    setLimit(clampLimit(next));
    setOffset(0);
  };

  const mutate = async (path: string, method: string, body: unknown) => {
    await api(path, { method, body });
    await load();
  };

  if (!canManageUsers(user.role)) {
    return (
      <>
        <PageHeader title="Users" sub="Account administration" />
        <Card>
          <Empty title="Owner access only" body="Only an owner can create, disable, delete or reset gateway accounts." />
        </Card>
      </>
    );
  }

  const window = pageWindow({ limit, offset, count: users.length, total });

  return (
    <>
      <PageHeader title="Users" sub="Owner-only account administration">
        <HealthPill />
        <button class="btn btn-primary" onClick={() => setDialog({ kind: "create" })}>New user</button>
      </PageHeader>

      <Card>
        <CardHead title="Accounts" sub={`${total} total`}>
          <label class="flex items-center gap-1.5 text-xs text-dim">
            Rows
            <select class="input w-auto" value={limit} onChange={(e) => changeLimit(Number((e.target as HTMLSelectElement).value))}>
              {PAGE_SIZES.map((size) => <option key={size} value={size}>{size}</option>)}
            </select>
          </label>
        </CardHead>
        {loading && users.length === 0 ? (
          <Empty title="Loading…" body="Reading accounts." />
        ) : users.length === 0 ? (
          <Empty title="No accounts" body="Create the first account with New user." />
        ) : (
          <table class="table">
            <thead>
              <tr>
                <th>User</th>
                <th>Role</th>
                <th>Status</th>
                <th>Last sign-in</th>
                <th>Created</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {users.map((record) => (
                <tr key={record.id}>
                  <td>
                    <div class="font-semibold">{record.display_name || record.username}</div>
                    <div class="text-[11px] text-dim">@{record.username}{record.email ? ` · ${record.email}` : ""}</div>
                  </td>
                  <td><span class="chip capitalize">{record.role}</span></td>
                  <td>
                    <span class={statusPill(record.status === "active" ? "ok" : "bad")}>{record.status}</span>
                    {record.must_change_password && <span class="ml-1 pill pill-warn">password change due</span>}
                  </td>
                  <td class="muted">{dateLabel(record.last_login_at)}</td>
                  <td class="muted">{dateLabel(record.created_at)}</td>
                  <td class="text-right whitespace-nowrap">
                    <button class="btn btn-sm" onClick={() => setDialog({ kind: "edit", user: record })}>Edit</button>{" "}
                    <button class="btn btn-sm" onClick={() => setDialog({ kind: "password", user: record })}>Reset password</button>{" "}
                    <button
                      class="btn btn-sm"
                      disabled={record.id === user.id}
                      title={record.id === user.id ? "You cannot disable your own account" : undefined}
                      onClick={() => {
                        const status = record.status === "active" ? "disabled" : "active";
                        void mutate(`/admin/api/users/${record.id}`, "PUT", { status })
                          .then(() => toast(`User ${status}`))
                          .catch((err) => toast(`Update failed: ${message(err)}`, "bad"));
                      }}
                    >
                      {record.status === "active" ? "Disable" : "Enable"}
                    </button>{" "}
                    <button
                      class="btn btn-sm btn-danger"
                      disabled={record.id === user.id}
                      title={record.id === user.id ? "You cannot delete your own account" : undefined}
                      onClick={() => setDialog({ kind: "delete", user: record })}
                    >
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        <Pager
          from={window.from}
          to={window.to}
          total={total}
          hasPrev={window.hasPrev}
          hasNext={window.hasNext}
          onPrev={() => setOffset(Math.max(0, offset - limit))}
          onNext={() => setOffset(offset + limit)}
        />
      </Card>

      {dialog?.kind === "create" && (
        <UserFormDialog
          mode="create"
          viewerID={user.id}
          onClose={() => setDialog(null)}
          onSaved={() => { setDialog(null); toast("User created"); void load(); }}
        />
      )}
      {dialog?.kind === "edit" && (
        <UserFormDialog
          mode="edit"
          target={dialog.user}
          viewerID={user.id}
          onClose={() => setDialog(null)}
          onSaved={() => { setDialog(null); toast("User updated"); void load(); }}
        />
      )}
      {dialog?.kind === "password" && (
        <ResetPasswordDialog
          target={dialog.user}
          currentUserID={user.id}
          onClose={() => setDialog(null)}
          onSaved={() => {
            setDialog(null);
            toast(dialog.user.id === user.id ? "Password changed" : "Password reset — the user must change it at next sign-in");
            void load();
          }}
        />
      )}
      {dialog?.kind === "delete" && (
        <DeleteUserDialog
          target={dialog.user}
          onClose={() => setDialog(null)}
          onDeleted={() => { setDialog(null); toast("User deleted"); void load(); }}
        />
      )}
    </>
  );
}

function UserFormDialog({
  mode,
  target,
  viewerID,
  onClose,
  onSaved,
}: {
  mode: "create" | "edit";
  target?: AdminUserRecord;
  viewerID: string;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [username, setUsername] = useState(target?.username ?? "");
  const [displayName, setDisplayName] = useState(target?.display_name ?? "");
  const [email, setEmail] = useState(target?.email ?? "");
  const [role, setRole] = useState(target?.role ?? "user");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const roleLocked = target != null && !canChangeRole(target.id, viewerID);

  const submit = async () => {
    setError(null);
    if (mode === "create") {
      const field: FieldError | null = validateNewUser({
        username,
        display_name: displayName,
        email,
        role,
        password,
      });
      if (field) {
        setError(field.message);
        return;
      }
    } else {
      // The edit PUT skips the username/password checks but still hits the same
      // display-name CHECK; an unknown server role must also be resolved first.
      const field = displayNameError(displayName) ?? roleError(role);
      if (field) {
        setError(field.message);
        return;
      }
    }
    setBusy(true);
    try {
      if (mode === "create") {
        await api("/admin/api/users", {
          method: "POST",
          body: { username: username.trim(), display_name: displayName.trim(), email: email.trim(), role, password },
        });
      } else {
        await api(`/admin/api/users/${target?.id}`, {
          method: "PUT",
          body: { display_name: displayName.trim(), email: email.trim(), role },
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
      title={mode === "create" ? "New user" : "Edit user"}
      confirmLabel={mode === "create" ? "Create" : "Save"}
      onConfirm={() => void submit()}
      onCancel={onClose}
      busy={busy}
    >
      <div class="space-y-3">
        {mode === "create" && (
          <label class="block text-xs font-semibold">
            Username
            <input class="input mt-1" value={username} disabled={busy} autoFocus
              onInput={(e) => setUsername((e.currentTarget as HTMLInputElement).value)} />
          </label>
        )}
        <label class="block text-xs font-semibold">
          Display name
          <input class="input mt-1" value={displayName} disabled={busy}
            onInput={(e) => setDisplayName((e.currentTarget as HTMLInputElement).value)} />
        </label>
        <label class="block text-xs font-semibold">
          Email
          <input class="input mt-1" type="email" value={email} disabled={busy}
            onInput={(e) => setEmail((e.currentTarget as HTMLInputElement).value)} />
        </label>
        <label class="block text-xs font-semibold">
          Role
          <select class="input mt-1" value={role} disabled={busy || roleLocked}
            onChange={(e) => setRole((e.target as HTMLSelectElement).value)}>
            {!ROLES.includes(role) && <option value={role}>{role} (unknown)</option>}
            {ROLES.map((value) => <option key={value} value={value}>{value}</option>)}
          </select>
          {roleLocked && <span class="mt-1 block text-[11px] font-normal text-dim">You cannot change your own role.</span>}
        </label>
        {mode === "create" && (
          <label class="block text-xs font-semibold">
            Temporary password
            <input class="input mt-1" type="password" autoComplete="new-password" value={password} disabled={busy}
              onInput={(e) => setPassword((e.currentTarget as HTMLInputElement).value)} />
            <span class="mt-1 block text-[11px] font-normal text-dim">12–128 characters. The user must change it at first sign-in.</span>
          </label>
        )}
        {error && <div role="alert" class="rounded-lg border border-bad bg-bad-soft px-3 py-2 text-xs text-bad">{error}</div>}
      </div>
    </Dialog>
  );
}

function ResetPasswordDialog({
  target,
  currentUserID,
  onClose,
  onSaved,
}: {
  target: AdminUserRecord;
  currentUserID: string;
  onClose: () => void;
  onSaved: () => void;
}) {
  const self = target.id === currentUserID;
  const [currentPassword, setCurrentPassword] = useState("");
  const [password, setPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async () => {
    if (self && !currentPassword) {
      setError("Current password is required");
      return;
    }
    const invalid = validateNewPassword(password, confirmation);
    if (invalid) {
      setError(invalid);
      return;
    }
    setBusy(true);
    setError(null);
    try {
      await api(`/admin/api/users/${target.id}/password`, {
        method: "POST",
        body: passwordResetBody({ self, password, currentPassword }),
      });
      onSaved();
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog title={`Reset password · @${target.username}`} confirmLabel="Reset" onConfirm={() => void submit()} onCancel={onClose} busy={busy}>
      <div class="space-y-3">
        <p class="m-0">
          {self
            ? "Your password changes immediately and other sessions are signed out."
            : "The current password stops working immediately. The account must change this temporary one at next sign-in."}
        </p>
        {self && (
          <label class="block text-xs font-semibold">
            Current password
            <input class="input mt-1" type="password" autoComplete="current-password" value={currentPassword} disabled={busy} autoFocus
              onInput={(e) => setCurrentPassword((e.currentTarget as HTMLInputElement).value)} />
          </label>
        )}
        <label class="block text-xs font-semibold">
          New password
          <input class="input mt-1" type="password" autoComplete="new-password" value={password} disabled={busy} autoFocus={!self}
            onInput={(e) => setPassword((e.currentTarget as HTMLInputElement).value)} />
        </label>
        <label class="block text-xs font-semibold">
          Confirm password
          <input class="input mt-1" type="password" autoComplete="new-password" value={confirmation} disabled={busy}
            onInput={(e) => setConfirmation((e.currentTarget as HTMLInputElement).value)} />
        </label>
        {error && <div role="alert" class="rounded-lg border border-bad bg-bad-soft px-3 py-2 text-xs text-bad">{error}</div>}
      </div>
    </Dialog>
  );
}

function DeleteUserDialog({
  target,
  onClose,
  onDeleted,
}: {
  target: AdminUserRecord;
  onClose: () => void;
  onDeleted: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const remove = async () => {
    setBusy(true);
    setError(null);
    try {
      await api(`/admin/api/users/${target.id}`, { method: "DELETE" });
      onDeleted();
    } catch (err) {
      setError(message(err));
      setBusy(false);
    }
  };

  return (
    <Dialog title={`Delete @${target.username}?`} confirmLabel="Delete user" danger onConfirm={() => void remove()} onCancel={onClose} busy={busy}>
      <div class="space-y-2">
        <p class="m-0">This removes the account, its sessions and its tokens. It cannot be undone.</p>
        {error && <div role="alert" class="rounded-lg border border-bad bg-bad-soft px-3 py-2 text-xs text-bad">{error}</div>}
      </div>
    </Dialog>
  );
}