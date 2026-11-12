import { useEffect, useState } from "preact/hooks";
import { api } from "../lib/api";
import { useUi } from "../components/ui";
import type { ChromeInfo, ChromeStatus } from "../lib/types";
import { Card, CardHead, Dialog, Empty, PageHeader } from "../components/ui";
import { HealthPill } from "../components/Shell";

export function ChromesView() {
  const { toast, setBanner } = useUi();
  const [status, setStatus] = useState<ChromeStatus | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<{ kind: "all" } | { kind: "one"; worker: ChromeInfo } | null>(null);
  const [busy, setBusy] = useState(false);
  const [working, setWorking] = useState<string | null>(null);

  const load = async () => {
    try {
      setStatus(await api<ChromeStatus>("/admin/api/chromes"));
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Chrome status failed");
    }
  };

  useEffect(() => {
    void load();
    const timer = setInterval(() => void load(), 5000);
    return () => clearInterval(timer);
  }, []);

  const post = async (body: Record<string, unknown>, label: string) => {
    setBusy(true);
    setWorking(label);
    try {
      await api("/admin/api/chromes", { method: "POST", body });
      toast(label);
      await load();
    } catch (err) {
      const message = err instanceof Error ? err.message : "Chrome action failed";
      setBanner(`${label} failed: ${message}`, () => void post(body, label));
      toast(message, "bad");
    } finally {
      setBusy(false);
      setWorking(null);
      setConfirm(null);
    }
  };

  const running = !!status && status.enabled && status.chromes.length > 0;
  const inUse = (status?.chromes || []).filter((c) => c.busy).length;
  const extracts = (status?.chromes || []).reduce((n, c) => n + (c.extracts || 0), 0);

  return (
    <>
      <PageHeader title="Chrome" sub="Captcha browser pool — start on demand, kill to free RAM">
        <HealthPill />
        <span class={`pill ${running ? "pill-ok" : "pill-mute"}`}>● {status?.state || "unknown"}</span>
        <span class="text-xs text-dim">auto-refresh 5s</span>
      </PageHeader>

      {error && <div class="mb-3 rounded-lg border border-bad bg-bad-soft px-3 py-2 text-xs text-bad">{error}</div>}

      <Card class="mb-3">
        <div class="flex flex-wrap items-center gap-6 px-[22px] py-6">
          <div>
            <div class="text-[30px] font-bold tracking-[-0.04em]">{running ? "Enabled" : status?.state || "Idle"}</div>
            <div class="text-xs text-dim">{running ? `${status?.chromes.length} workers allocated` : "No Chrome processes allocated"}</div>
          </div>
          <Stat value={status?.ready ?? 0} label="ready tokens" />
          <Stat value={`${running ? status?.chromes.length : 0} / ${running ? status?.chromes.length : 0}`} label="workers running" />
          <Stat value={inUse} label="in use now" />
          <Stat value={extracts} label="captcha extracts" />
          <div class="ml-auto flex gap-2">
            <button class="btn btn-primary" disabled={busy} onClick={() => void post({ mode: "start", all: true }, "Starting pool")}>
              {working === "Starting pool" ? "Starting…" : "Start All"}
            </button>
            <button
              class="btn btn-danger"
              disabled={busy || !running}
              title={running ? "Stops every worker" : "Nothing to kill — no workers are running"}
              onClick={() => setConfirm({ kind: "all" })}
            >
              Kill All…
            </button>
          </div>
        </div>
      </Card>

      <Card>
        <CardHead title="Workers" sub={`${status?.chromes.length || 0} allocated`} />
        {!status?.chromes.length ? (
          <Empty title="No Chrome processes allocated" body="Start All launches the pool; each worker can be killed to free its RAM." />
        ) : (
          <table class="table">
            <thead>
              <tr><th class="w-14">#</th><th>State</th><th>PID</th><th>Extracts</th><th>Last OK</th><th /></tr>
            </thead>
            <tbody>
              {status.chromes.map((c) => (
                <tr key={c.index}>
                  <td><span class="chip">{c.index}</span></td>
                  <td>
                    <span class={`pill ${c.killed ? "pill-bad" : c.busy ? "pill-warn" : "pill-ok"}`}>
                      {c.killed ? "killed" : c.busy ? "in use" : "running"}
                    </span>{" "}
                    {c.warmed && <span class="pill pill-mute">warm</span>}
                  </td>
                  <td class="mono">{c.pid}</td>
                  <td>{c.extracts}</td>
                  <td class="muted">{c.last_ok || "—"}</td>
                  <td class="text-right">
                    <button class="btn btn-sm" disabled={busy} onClick={() => setConfirm({ kind: "one", worker: c })}>Kill…</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>

      {confirm?.kind === "all" && (
        <Dialog
          title="Kill all Chrome workers?"
          confirmLabel="Kill all"
          danger
          busy={busy}
          onCancel={() => setConfirm(null)}
          onConfirm={() => void post({ mode: "kill", all: true }, "Killing all workers")}
        >
          This stops <b class="text-fg">{status?.chromes.length || 0} workers</b> and frees their RAM.
        </Dialog>
      )}
      {confirm?.kind === "one" && (
        <Dialog
          title={`Kill Chrome #${confirm.worker.index}?`}
          confirmLabel="Kill worker"
          danger
          busy={busy}
          onCancel={() => setConfirm(null)}
          onConfirm={() => void post({ mode: "kill", index: confirm.worker.index }, `Killing worker #${confirm.worker.index}`)}
        >
          This stops worker <b class="text-fg">#{confirm.worker.index}</b> (PID {confirm.worker.pid}).
          {confirm.worker.busy ? " It is in use, so this interrupts the current extraction." : ""}
        </Dialog>
      )}
    </>
  );
}

function Stat({ value, label }: { value: number | string; label: string }) {
  return (
    <div class="border-l border-line px-[18px] text-center">
      <div class="text-2xl font-bold tabular-nums tracking-[-0.03em]">{value}</div>
      <div class="text-[10px] font-semibold uppercase tracking-[0.05em] text-dim">{label}</div>
    </div>
  );
}
