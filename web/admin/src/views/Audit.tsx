import { useEffect, useRef, useState } from "preact/hooks";
import { api } from "../lib/api";
import { useUi } from "../components/ui";
import { Card, CardHead, Empty, PageHeader, Pager } from "../components/ui";
import { isOwnerRole, type AdminUser } from "../lib/auth";
import {
  auditMatches,
  auditTime,
  detailRows,
  fetchAuditPage,
  firstAuditRequest,
  nextAuditRequest,
  previousAuditRequest,
  type AuditEntry,
  type AuditPage,
  type AuditRequest,
} from "../lib/audit";
import { DEFAULT_LIMIT, PAGE_SIZES, clampLimit, message } from "../lib/admin";

function shortID(value: string | null): string {
  if (!value) return "—";
  return value.length > 12 ? `${value.slice(0, 8)}…` : value;
}

/** The redacted details cell. `details` was allowlisted when the page parsed. */
export function AuditDetails({ entry }: { entry: AuditEntry }) {
  const rows = detailRows(entry);
  if (rows.length === 0) return <span class="text-dim">—</span>;
  return (
    <div class="flex flex-wrap gap-1">
      {rows.map((row, index) => (
        <span class="chip" key={`${row.key}-${index}`}>{row.key}: {row.value}</span>
      ))}
    </div>
  );
}

export function AuditView({ user }: { user: AdminUser }) {
  const { setBanner } = useUi();
  const [request, setRequest] = useState<AuditRequest>(() => firstAuditRequest(DEFAULT_LIMIT));
  const [page, setPage] = useState<AuditPage | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [filter, setFilter] = useState("");
  const requestId = useRef(0);

  const load = async (next: AuditRequest) => {
    const thisRequest = ++requestId.current;
    setLoading(true);
    setLoadError(null);
    try {
      const body = await fetchAuditPage(next, (path) => api<unknown>(path));
      if (thisRequest !== requestId.current) return;
      setPage(body);
      setBanner(null, undefined, "audit");
    } catch (err) {
      if (thisRequest !== requestId.current) return;
      setPage(null);
      setLoadError(message(err));
      setBanner(`Audit unavailable: ${message(err)}`, () => void load(next), "audit");
    } finally {
      if (thisRequest === requestId.current) setLoading(false);
    }
  };

  useEffect(() => {
    void load(request);
  }, [request]);

  // The router already refuses an unauthorised ?page=, so this is the render
  // half of the same owner-only gate.
  if (!isOwnerRole(user.role)) {
    return (
      <>
        <PageHeader title="Audit" sub="Owner-only audit trail" />
        <Card>
          <Empty title="Owner access only" body="Only an owner can read the audit log." />
        </Card>
      </>
    );
  }

  const changeLimit = (value: number) => setRequest(firstAuditRequest(clampLimit(value)));
  const reset = () => setRequest(firstAuditRequest(request.limit));
  const previous = page ? previousAuditRequest(request, page) : null;
  const next = page ? nextAuditRequest(request, page) : null;
  const entries = (page?.entries ?? []).filter((entry) => auditMatches(entry, filter));
  const filtering = filter.trim().length > 0;
  const loaded = page?.entries.length ?? 0;
  const from = loaded ? (request.depth - 1) * request.limit + 1 : 0;
  const to = from ? from + loaded - 1 : 0;

  return (
    <>
      <PageHeader title="Audit" sub="Owner-only audit trail">
        <button class="btn" onClick={reset} disabled={loading}>Newest</button>
      </PageHeader>

      <Card>
        <CardHead title="Entries" sub={page ? `${loaded} on this page` : undefined}>
          <input
            class="input w-56"
            placeholder="Filter this page"
            value={filter}
            aria-label="Filter audit entries"
            onInput={(e) => setFilter((e.currentTarget as HTMLInputElement).value)}
          />
          <label class="flex items-center gap-1.5 text-xs text-dim">
            Rows
            <select class="input w-auto" value={request.limit} onChange={(e) => changeLimit(Number((e.target as HTMLSelectElement).value))}>
              {PAGE_SIZES.map((size) => <option key={size} value={size}>{size}</option>)}
            </select>
          </label>
        </CardHead>

        {loadError ? (
          <Empty title="Audit unavailable" body={loadError} />
        ) : loading && !page ? (
          <Empty title="Loading…" body="Reading the audit log." />
        ) : entries.length === 0 ? (
          <Empty
            title={filtering ? "No matching entries" : "No audit entries"}
            body={filtering ? "No loaded entry matches the filter." : "No audit events have been recorded yet."}
          />
        ) : (
          <table class="table">
            <thead>
              <tr>
                <th>Time</th>
                <th>Actor</th>
                <th>Action</th>
                <th>Target</th>
                <th>Context</th>
                <th>Details</th>
              </tr>
            </thead>
            <tbody>
              {entries.map((entry) => (
                <tr key={entry.id}>
                  <td class="muted whitespace-nowrap">{auditTime(entry.ts)}</td>
                  <td>
                    <span class="chip">{entry.actor_type}</span>{" "}
                    <span class="mono text-[11px] text-dim" title={entry.actor_id ?? undefined}>{shortID(entry.actor_id)}</span>
                  </td>
                  <td class="font-semibold mono">{entry.action}</td>
                  <td>
                    <span class="chip">{entry.target_type}</span>{" "}
                    <span class="mono text-[11px] text-dim" title={entry.target_id ?? undefined}>{shortID(entry.target_id)}</span>
                  </td>
                  <td class="muted text-[11px]">
                    {entry.ip || "—"}
                    {entry.request_id ? ` · ${shortID(entry.request_id)}` : ""}
                  </td>
                  <td><AuditDetails entry={entry} /></td>
                </tr>
              ))}
            </tbody>
          </table>
        )}

        <Pager
          from={from}
          to={to}
          hasPrev={!!previous}
          hasNext={!!next}
          onPrev={() => previous && setRequest(previous)}
          onNext={() => next && setRequest(next)}
        />
      </Card>
    </>
  );
}
