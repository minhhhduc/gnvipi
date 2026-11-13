import type { ComponentChildren } from "preact";
import { useState } from "preact/hooks";
import { api, ApiError } from "../lib/api";
import {
  passwordChangeError,
  parseAuthSession,
  type AdminUser,
  type AuthSession,
} from "../lib/auth";

const LOGIN_PATH = "/admin/api/auth/login";
const PASSWORD_PATH = "/admin/api/auth/password";

function AuthFrame({
  title,
  sub,
  children,
}: {
  title: string;
  sub: string;
  children: ComponentChildren;
}) {
  return (
    <main class="grid min-h-screen place-items-center px-5 py-10">
      <div class="w-full max-w-[400px]">
        <div class="mb-5 flex items-center justify-center gap-2">
          <span class="grid size-9 place-items-center rounded-[10px] bg-accent text-sm font-extrabold text-white">GW</span>
          <b class="text-lg tracking-[-0.02em]">Gateway</b>
        </div>
        <section class="card p-6" aria-labelledby="auth-title">
          <h1 id="auth-title" class="text-xl font-bold tracking-[-0.025em]">{title}</h1>
          <p class="mt-1 text-[13px] text-dim">{sub}</p>
          {children}
        </section>
      </div>
    </main>
  );
}

export function LoginScreen({
  onAuthenticated,
}: {
  onAuthenticated: (session: AuthSession) => void;
}) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const submit = async (event: SubmitEvent) => {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      const response = await api<unknown>(LOGIN_PATH, {
        method: "POST",
        body: { username, password },
        redirectOn401: false,
      });
      const session = parseAuthSession(response);
      setPassword("");
      onAuthenticated(session);
    } catch (err) {
      setError(err instanceof ApiError && err.status === 401
        ? "Invalid username or password"
        : err instanceof Error ? err.message : "Sign in failed");
    } finally {
      setBusy(false);
    }
  };

  return (
    <AuthFrame title="Sign in" sub="Use your gateway account to open the admin console.">
      <form class="mt-5 space-y-4" onSubmit={(event) => void submit(event)}>
        <div>
          <label for="login-username" class="mb-1 block text-xs font-semibold">Username</label>
          <input
            id="login-username"
            class="input"
            type="text"
            autoComplete="username"
            autoFocus
            required
            disabled={busy}
            value={username}
            onInput={(event) => setUsername((event.currentTarget as HTMLInputElement).value)}
          />
        </div>
        <div>
          <label for="login-password" class="mb-1 block text-xs font-semibold">Password</label>
          <input
            id="login-password"
            class="input"
            type="password"
            autoComplete="current-password"
            required
            disabled={busy}
            value={password}
            onInput={(event) => setPassword((event.currentTarget as HTMLInputElement).value)}
          />
        </div>
        {error && (
          <div class="rounded-lg border border-bad bg-bad-soft px-3 py-2.5 text-xs text-bad" role="alert">{error}</div>
        )}
        <button class="btn btn-primary w-full" type="submit" disabled={busy || !username.trim() || !password}>
          {busy ? "Signing in…" : "Sign in"}
        </button>
      </form>
    </AuthFrame>
  );
}

export function PasswordChangeScreen({
  user,
  onChanged,
  onLogout,
}: {
  user: AdminUser;
  onChanged: (user: AdminUser) => void;
  onLogout: () => Promise<void>;
}) {
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const submit = async (event: SubmitEvent) => {
    event.preventDefault();
    const validationError = passwordChangeError(currentPassword, newPassword, confirmation);
    if (validationError) {
      setError(validationError);
      return;
    }
    setBusy(true);
    setError("");
    try {
      await api(PASSWORD_PATH, {
        method: "POST",
        body: { current_password: currentPassword, new_password: newPassword },
      });
      onChanged({ ...user, must_change_password: false });
    } catch (err) {
      setError(err instanceof Error ? err.message : "Password change failed");
    } finally {
      setBusy(false);
    }
  };

  const logout = async () => {
    setBusy(true);
    setError("");
    try {
      await onLogout();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Sign out failed");
      setBusy(false);
    }
  };

  return (
    <AuthFrame title="Change your password" sub={`Welcome, ${user.display_name || user.username}. Replace the temporary password before continuing.`}>
      <form class="mt-5 space-y-4" onSubmit={(event) => void submit(event)}>
        <div>
          <label for="current-password" class="mb-1 block text-xs font-semibold">Current password</label>
          <input
            id="current-password"
            class="input"
            type="password"
            autoComplete="current-password"
            autoFocus
            required
            disabled={busy}
            value={currentPassword}
            onInput={(event) => setCurrentPassword((event.currentTarget as HTMLInputElement).value)}
          />
        </div>
        <div>
          <label for="new-password" class="mb-1 block text-xs font-semibold">New password</label>
          <input
            id="new-password"
            class="input"
            type="password"
            autoComplete="new-password"
            minLength={12}
            maxLength={128}
            required
            disabled={busy}
            value={newPassword}
            onInput={(event) => setNewPassword((event.currentTarget as HTMLInputElement).value)}
          />
          <p class="mt-1 text-[11px] text-dim">Use 12–128 characters. The server checks common passwords and account details.</p>
        </div>
        <div>
          <label for="confirm-password" class="mb-1 block text-xs font-semibold">Confirm new password</label>
          <input
            id="confirm-password"
            class="input"
            type="password"
            autoComplete="new-password"
            minLength={12}
            maxLength={128}
            required
            disabled={busy}
            value={confirmation}
            onInput={(event) => setConfirmation((event.currentTarget as HTMLInputElement).value)}
          />
        </div>
        {error && (
          <div class="rounded-lg border border-bad bg-bad-soft px-3 py-2.5 text-xs text-bad" role="alert">{error}</div>
        )}
        <div class="flex gap-2">
          <button class="btn flex-1" type="button" disabled={busy} onClick={() => void logout()}>Sign out</button>
          <button class="btn btn-primary flex-1" type="submit" disabled={busy}>
            {busy ? "Saving…" : "Change password"}
          </button>
        </div>
      </form>
    </AuthFrame>
  );
}
