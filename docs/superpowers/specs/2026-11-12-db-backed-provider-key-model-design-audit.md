# Design audit: DB-backed provider, key and model rework

Date: 2026-11-12
Audited design: `docs/superpowers/specs/2026-11-12-db-backed-provider-key-model-design.md`
Scope: provider/key/model schema, authentication, authorization, routing,
secret handling, migrations, operations and v2 compatibility.
Evidence: the revised specification plus the current repository at
`feat/183-admin-ui-rebuild` (`4f098ab`). No application code, migration or
runtime behavior was changed by this audit.

## Verdict

**READY WITH DOCUMENTED FOLLOW-UPS.**

The design is implementation-ready after the revisions below. Every critical
and high finding is resolved in the specification. The remaining follow-ups are
explicit phase-2 items or documented product limitations; none blocks the first
cutover. This is design readiness, not implementation readiness: there are no
migrations, handlers, or tests in the repository yet, and the required test
suite in §13 of the spec must be written with the implementation.

## Method

1. Read the existing specification and compared it with the current Go types
   and handlers.
2. Checked every table/column, foreign key, check, unique index, trigger and
   cascade against the API operations.
3. Traced authentication, key resolution, routing, fallback, stats and audit
   paths.
4. Checked migration/bootstrap concurrency, secret handling and rollback.
5. Classified findings by production impact and resolved them in the existing
   specification rather than creating an alternative design.

Repository discrepancies found during the audit:

| Area | Current repository | Consequence |
|---|---|---|
| Auth schemes | `internal/gateway/provider/provider.go:24-51` has `none`, `bearer`, `x-api-key`, plus `HeaderName`. No `header` or `basic`. | The new internal `header`/`basic` schemes need an adapter; v2 `x-api-key` must map to `header` + `auth_header_name=x-api-key`. |
| v2 header shape | Existing v2 supports `x-api-key` plus `auth.header_name`. | Do not expose `header` as a new v2 scheme; map it to `x-api-key` + `header_name`. |
| Settings | `cmd/serve/admin_handlers.go:69-210`; `cycleConfig` is `{name,models}`. | JSON contracts must preserve these shapes. |
| Fallback | `internal/gateway/dispatch/fallback.go:142-170` defaults to 408/429/5xx. | The spec documents that default; it is not a new behavior. |
| Wildcards | `internal/gateway/routing/resolver.go:874-900` uses `prefix + suffix`, `*` and `/*` overrides. | The spec must document the exact substitution rules. |
| Hidden | `resolver.go` treats hidden as not routable. | The spec must not claim hidden is listing-only. |
| Stats | `internal/provider/nvidia/stats.go:61-71` has no user/key/provider/request ID; JSONL store in `stats_log.go`. | Additive fields are required; cross-instance aggregation is not available. |
| Dev compose | Root `docker-compose.yml` already exists for `serve`; no `db` service. | Extend it; do not create a second compose file. |
| Dev script | `scripts/run.ps1:128-130` hardcodes `local-gateway`. | #212 must require `GNVIPI_GATEWAY_TOKEN`. |
| Migrations | No `migrations/` directory or SQL files exist. | The schema in the spec is the implementation contract, not an applied migration. |
| Admin routes | `cmd/serve/admin_api.go:174-218` has no users/tokens/audit routes. | New routes are additive; existing provider/model/stats routes must stay. |

## Critical findings

### C1 — Credential CHECK contradicted clear-secret/disable

**Wrong:** The old schema required every non-`none` key to have a credential,
including disabled keys, while the API allowed `clear_secret` when disabling a
key.  
**Why it matters:** The API and DB cannot both be right. Every clear-secret
transaction would fail, or the API would silently leave a credential behind.  
**Production failure mode:** A user disables a compromised key, the request
returns 500/422, and the credential remains usable.  
**Resolution:** One source for an enabled non-`none` key; zero sources allowed
only when disabled or `none`:

```sql
CHECK (num_nonnulls(secret_enc, key_env, key_ref) <= 1)
CHECK (auth_scheme = 'none' OR NOT enabled
       OR num_nonnulls(secret_enc, key_env, key_ref) = 1)
```

`clear_secret` is legal only when the final row is disabled or has another
source. The API returns 422 `field_errors.credential` otherwise.  
**Change:** schema + API + docs. Resolved in spec §4.4, §4.9, §9.3.

### C2 — Key/model scope could drift and route across users

**Wrong:** The old trigger copied `provider_keys.provider_id/owner_id` into
`key_models` on insert, but nothing stopped a later change to the parent key's
owner or provider.  
**Why it matters:** `key_models.owner_id` is the routing boundary. Drift could
make user A's model mapping resolve through user B's key, or attach a mapping
to a provider it does not belong to. The AAD also binds `owner_id`, so a scope
change would make secrets undecryptable.  
**Production failure mode:** Cross-user key use or a key that cannot decrypt
after an administrative edit.  
**Resolution:** `provider_keys.provider_id/owner_id` are immutable; key scope
changes require delete/recreate. A generated non-null `scope_owner` plus a
composite foreign key enforces `key_models` scope for owned and shared keys:

```sql
provider_keys.scope_owner = COALESCE(owner_id, zero_uuid)
UNIQUE (id, provider_id, scope_owner)
key_models FOREIGN KEY (key_id, provider_id, scope_owner)
  REFERENCES provider_keys(id, provider_id, scope_owner) ON DELETE CASCADE
```

The insert trigger still fills the derived columns; the update trigger rejects
drift.  
**Change:** schema + docs. Resolved in spec §4.4, §4.5.

### C3 — Config version bumps depended on every handler remembering

**Wrong:** The old design said "every admin write calls `bump_config_version()`".
A new handler, a cascade, or a settings import could forget.  
**Why it matters:** A stale snapshot means a deleted key remains routable, a
new key is invisible, or a disabled provider keeps serving.  
**Production failure mode:** Secrets are revoked in the UI but remain active
until a restart; or a new key appears to save but never routes.  
**Resolution:** `config_state` plus `AFTER INSERT OR UPDATE OR DELETE FOR EACH
STATEMENT` triggers on `providers`, `provider_keys`, `key_models`,
`model_state`, `settings`, and `AFTER DELETE ON users`. The trigger is
`SECURITY DEFINER` and owns the counter update. Application code no longer
bumps manually.  
**Change:** schema + docs. Resolved in spec §4.7.

### C4 — Snapshot rebuild had no consistency boundary

**Wrong:** The old design said "reload the tables" without an isolation level
or partial-publish rule.  
**Why it matters:** A multi-table write could be observed half-applied (key
present, model absent; provider disabled, key still enabled), or a decode error
could publish an empty/partial snapshot.  
**Production failure mode:** Requests route to a deleted key or fail closed
during an ordinary admin edit.  
**Resolution:** Load under `REPEATABLE READ READ ONLY`; validate every row;
publish atomically only after commit; keep the last good snapshot on any
failure; compare `config_state.version` after load and reload immediately if it
advanced. `LISTEN` is reconnect-aware; 30s poll is the fallback.  
**Change:** app + docs. Resolved in spec §4.7, §8.2.

### C5 — Sensitive configuration could store plaintext credentials

**Wrong:** The old design allowed arbitrary `proxy_url`, `headers`,
`extra_body` and `extensions` while claiming secrets only live in encrypted
columns. `proxy_url` can contain `user:pass@`; headers can contain
`Authorization`; extra body can contain `api_key`.  
**Why it matters:** These fields are returned by v2 `LegacyView`, written to
audit/backup, and are not encrypted. The "no plaintext credential" guarantee
would be false.  
**Production failure mode:** A DB backup or admin API response leaks a
provider credential that was never encrypted.  
**Resolution:** Reject userinfo in `base_url`/`proxy_url`; proxy credentials
come from env/deployment secrets. `headers` rejects credential headers and
credential-like names. `extra_body`/`extensions` reject credential-like keys.
Only `provider_keys` may hold credentials. Test responses are truncated and
redacted.  
**Change:** API validation + docs. Resolved in spec §4.6, §6.8, §9.6, §11.

### C6 — `must_change_password` was not enforced

**Wrong:** The old design stored the flag but did not state which routes check
it, or whether gateway/admin tokens bypass it.  
**Why it matters:** A temporary password or owner reset would leave a fully
usable account and gateway token.  
**Production failure mode:** A user with a known temporary password keeps
routing traffic without ever changing it.  
**Resolution:** Middleware checks the flag for sessions and both token kinds.
Only `GET /admin/api/me`, `POST /admin/api/me/password` and logout are allowed.
Gateway/admin API returns 403 `password_change_required`. A successful change
clears the flag and revokes other sessions.  
**Change:** app + API + docs. Resolved in spec §6.3, §7.

### C7 — Last-owner check was application-only and racy

**Wrong:** The old design used an advisory lock and a count in application code.
Two concurrent demotions could each see the other owner and both succeed.  
**Why it matters:** The system can end with zero active owners and no recovery
path except direct DB surgery.  
**Production failure mode:** Two owners are disabled/demoted at the same time;
nobody can administer users.  
**Resolution:** A `BEFORE UPDATE OF role,status` / `BEFORE DELETE` trigger takes
`pg_advisory_xact_lock(hashtext('gnvipi:owners'))` and counts the remaining
active owners excluding the row. User-management transactions use `READ
COMMITTED`, so the second waiter sees the first commit. Bootstrap uses a
separate `gnvipi:bootstrap` lock and an empty-`users` check.  
**Change:** schema + app + docs. Resolved in spec §4.5, §6.7.

### C8 — Auth enum and v2 mapping were incompatible

**Wrong:** The schema introduced `header`/`basic`, while the Go type only had
`none`/`bearer`/`x-api-key`; the old compatibility text mapped `header` to a v2
scheme that old clients do not know.  
**Why it matters:** Existing `x-api-key` providers would break or be
uneditable, and a new `header` provider would be rejected by the current
validator.  
**Production failure mode:** After import, a working Anthropic provider fails
with "invalid auth.scheme" or loses its header behavior.  
**Resolution:** Internal DB scheme `header` maps to v2 `x-api-key` +
`auth.header_name` (omit when the name is `x-api-key`). `basic` is additive and
documented as an old-client limitation. Existing `x-api-key` maps to `header` +
`x-api-key`.  
**Change:** adapter + compatibility docs. Resolved in spec §2, §9.4,
Appendix A.

### C9 — Secret envelope lacked nonce/key-version/AAD validation

**Wrong:** The old design specified AES-GCM but did not constrain nonce length,
key-version presence, AAD binding or decrypt-failure behavior.  
**Why it matters:** A truncated nonce, unknown key version, or ciphertext moved
between keys/users could cause silent corruption or a confusing 500. AAD
binding is what prevents a ciphertext from being replayed under another key.  
**Production failure mode:** After a restore or manual row edit, a key either
cannot decrypt or, worse, decrypts under the wrong identity.  
**Resolution:** 12-byte nonce, positive `secret_kver`, AAD binding key id +
scope owner + scheme + key version, hard failure on unknown version/decrypt,
no fallback. `secret_last4` is derived at save and cleared with the secret.  
**Change:** schema + crypto + tests. Resolved in spec §4.8, §6.8.

### C10 — Credential precedence/fallback was ambiguous and unsafe

**Wrong:** The old design allowed `secret_enc`, `key_env` and `key_ref` to
coexist with "precedence" and did not say what happens when the preferred source
is missing or fails to decrypt.  
**Why it matters:** A decrypt failure could fall back to a different credential
without the operator knowing, or a missing env var could silently use a stale
ref. That is credential substitution, not resilience.  
**Production failure mode:** A rotated/broken secret silently sends traffic with
an old env credential; audit cannot explain which credential was used.  
**Resolution:** Exactly one source per enabled key; no coexistence. Resolution
is deterministic by the one non-null column. Missing/decrypt failure is a hard
key error, excluded from routing, audited; no fallback to another source or
user.  
**Change:** schema + app + docs. Resolved in spec §4.4, §6.8.

## High findings

### H1 — JSONB columns had no shape contract

**Wrong:** `endpoint_paths`, `headers`, `extra_body`, `extensions`, `timeouts`,
`fallbacks`, `fallback_statuses` and `settings.value` were `jsonb` with no
documented structure.  
**Why it matters:** A malformed `fallbacks` object or a string `extra_body`
would panic or be silently ignored at routing time.  
**Failure mode:** A bad admin write takes down model resolution for one user or
the whole instance.  
**Resolution:** DB `jsonb_typeof` checks and size limits; a per-column contract
table; app validation with `field_errors`; `extra_body` forbids
`model`/`stream`; `fallbacks` is exact aliases only.  
**Change:** schema + API + docs. Resolved in spec §4.6.

### H2 — Routing/fallback semantics were underspecified

**Wrong:** Exact/wildcard precedence, wildcard substitution, fallback status
defaults, cycles, cross-provider behavior, and max depth were not defined
against the current resolver.  
**Why it matters:** Different implementations would route the same alias
differently, and a fallback cycle could recurse or loop.  
**Failure mode:** A fallback chain loops, a wildcard returns the wrong upstream
model, or a user's fallback reaches another user's key.  
**Resolution:** Explicit order (personal exact, personal wildcard, shared
exact, shared wildcard); deterministic tie-breaks; documented suffix
substitution; exact-only fallbacks; visited set and max 8; cross-provider
allowed, cross-user impossible; empty statuses = 408/429/5xx; network errors
always trigger.  
**Change:** app + docs. Resolved in spec §8.1.

### H3 — `hidden`/deleted semantics contradicted the resolver

**Wrong:** One section implied `hidden` only affects listing; the resolver
returns 404 for hidden models.  
**Why it matters:** A "hidden" model would be documented as routable but fail at
runtime, or worse, an implementation would make it routable and bypass the
operator's intent.  
**Failure mode:** A hidden model still serves traffic or a documented route
404s.  
**Resolution:** `hidden` = not listed and not routable; `deleted_at` is a soft
delete that wins and applies to everyone.  
**Change:** docs + tests. Resolved in spec §8.1.

### H4 — Direct provider test could leak secrets or enable SSRF

**Wrong:** The test endpoint and `base_url` creation had no explicit redaction
or private-address rule in the old design.  
**Why it matters:** Upstream error bodies often echo credentials; an
owner/admin-supplied `base_url` can target link-local/metadata services.  
**Failure mode:** A test response writes a key into the page/error log, or the
server is used to probe internal networks.  
**Resolution:** Owner/admin only; HTTP(S) only; private/link-local requires an
explicit deployment allowlist; request headers never echoed; response headers
and body truncated/redacted; no secret in `message`/`error`.  
**Change:** API + docs. Resolved in spec §6.8, §6.9, §9.6.

### H5 — Audit details could contain secrets; audit had no subject columns

**Wrong:** Free-form `details jsonb` plus no explicit `user_id`/`key_id`/
`provider_id` made it easy to dump request headers or key material, and hard to
authorize/query by subject.  
**Why it matters:** Audit is owner-readable and long-lived; it is the worst
place for a leaked secret.  
**Failure mode:** An audit row contains a provider header or env ref; every
owner can read it forever.  
**Resolution:** Explicit subject columns (no FKs, preserve after delete);
allowlisted details; 8 KiB cap; indexes for subject queries; audit written in
the same transaction as the mutation.  
**Change:** schema + app + docs. Resolved in spec §4.3, §8.4.

### H6 — Stats had no user/key attribution and no privacy contract

**Wrong:** `RequestEvent` only has time/model/tokens/latency; per-user stats
could not be filtered.  
**Why it matters:** `/admin/api/stats` for a `user` role must not show another
user's requests. Without a persisted subject, filtering is impossible.  
**Failure mode:** A regular user sees aggregate or other users' request
metadata; or the API silently returns global stats.  
**Resolution:** Add additive `request_id`, `user_id`, `key_id`, `provider_id`,
`status`; never store prompts/bodies/headers; per-process JSONL limitation is
documented; user filtering is mandatory. Cross-instance aggregation is phase 2.
**Change:** app + API + docs. Resolved in spec §8.4.

### H7 — Migration/runtime grants and schema guard were incomplete

**Wrong:** The app role needed `SELECT` on `schema_migrations` and
`config_state`, and `EXECUTE` on the version trigger, but the old grant list did
not include them.  
**Why it matters:** Startup would fail the schema guard, or every config write
would fail on the trigger.  
**Failure mode:** Clean deploy starts, then every admin save returns permission
denied; or startup cannot read the applied version.  
**Resolution:** Explicit grants in §5; app role has no DDL; trigger function is
`SECURITY DEFINER` with a fixed `search_path`; migration role separate.  
**Change:** migrations + docs. Resolved in spec §5.

### H8 — Bootstrap and password reset were not concurrency-safe

**Wrong:** "First owner only while users is empty" was stated but not locked;
password reset did not define `must_change_password` or token revocation.  
**Why it matters:** Two bootstrap processes can race, or a reset leaves old
sessions/tokens usable.  
**Failure mode:** Two owners created from one bootstrap, or a compromised
session survives a password reset.  
**Resolution:** Bootstrap advisory lock + empty check in one transaction;
reset sets `must_change_password`, revokes sessions/tokens, audits; no password
returned or logged.  
**Change:** CLI + API + docs. Resolved in spec §4.5, §6.3, §6.7.

### H9 — Alias collisions and wildcard collisions were treated as errors in one
path and warnings in another

**Wrong:** `CheckCollisions` rejects some collisions; the resolver warns and
keeps the first. The old spec did not say which is authoritative for the DB.  
**Why it matters:** An admin save could succeed with a collision that later
routes unpredictably.  
**Failure mode:** Two providers claim `vendor/*`; one user's route changes after
a restart or another write.  
**Resolution:** DB uniqueness is per `(provider, owner, alias)`; cross-provider
collisions are allowed and resolved deterministically; the resolver emits a
warning. This matches the existing resolver's "keep first" behavior.  
**Change:** schema + app + docs. Resolved in spec §4.4, §8.1.

### H10 — The existing compose file would be duplicated

**Wrong:** The old spec said "add `docker-compose.yml` at the repo root", but one
already exists for `serve`.  
**Why it matters:** A second compose file or an overwrite would break the
current local run path and port 8080 ownership.  
**Failure mode:** `docker compose up` loses the gateway service or the DB
service is not on the same network.  
**Resolution:** Extend the existing file with a `db` service and
`depends_on`/DSN; keep the `serve` service; run.ps1 checks readiness.  
**Change:** compose + docs. Resolved in spec §12.2, Appendix C; #212.

## Medium findings

| ID | Finding | Resolution | Change |
|---|---|---|---|
| M1 | `protocol` is free text with no enum. | Keep text; app validates against the adapter registry; a new adapter is a deliberate release. | app/docs |
| M2 | `base_url` DB regex only accepts lowercase `http`/`https`. | API normalizes the scheme before insert; uppercase input is canonicalized, not stored. | app |
| M3 | Existing providers may put credentials in `headers` or `proxy_url`. | Import fails with a field error; move to `provider_keys`/env. This is a deliberate security boundary. | import/docs |
| M4 | `basic` is new and old v2 clients cannot edit it. | Documented additive limitation; LegacyView returns `basic`; old clients must not PUT it. | API/docs |
| M5 | `fallback_statuses` empty semantics were not stated. | Empty = 408/429/5xx default; non-empty = exact list; network errors always trigger. | docs |
| M6 | Audit cursor `(ts,id)` can skip/duplicate rows if timestamps tie. | Cursor includes both; order is `ts DESC, id DESC`; client uses opaque cursor, not raw offset. | API/docs |
| M7 | `users.email` unique nullable allows multiple NULLs. | Intended: email is optional; only non-null emails are unique. | docs |
| M8 | `audit_log` subject columns are not FKs. | Intentional; audit must survive deletion. `target_type/target_id` remain canonical. | docs |
| M9 | `provider_keys` unique indexes use `NULL` owner partials. | Correct for shared vs personal scopes; duplicate name race is caught by the unique index, mapped to `field_errors.name`. | app/docs |
| M10 | `key_models.alias` uniqueness is per provider/owner, not per key. | Intended: one alias per provider/owner avoids ambiguous routing; different keys on the same provider cannot claim the same alias. | schema/docs |
| M11 | `model_state` is global. | Owner/admin-only action; audited; hidden/deleted applies to all users. | API/docs |
| M12 | `settings.value` has no DB shape check beyond object. | App validates per-key shapes; DB check prevents non-object corruption. | app/docs |
| M13 | Stats JSONL is per-process. | Documented phase-1 limitation; phase-2 `request_events` table if cross-instance aggregation is required. | docs |
| M14 | Audit growth is unbounded until phase 2. | Admin actions are low volume; phase-2 retention job; no app-role DELETE. | docs |
| M15 | `sessions`/`login_attempts` need cleanup before phase 2. | Phase-1 startup + 6-hour cleanup; no maintenance role required. | app/docs |
| M16 | `schema_migrations` is created by golang-migrate. | Grant SELECT; app never creates it; schema guard reads it. | migrations/docs |
| M17 | `config_state` boolean PK is unconventional. | Singleton enforced by `CHECK (id)` and PK; one row only. | schema/docs |
| M18 | `system_state` is now only an import marker. | Name retained for compatibility; routing version moved to `config_state`. | schema/docs |
| M19 | `key_models_alias_idx` plus unique indexes may be redundant. | Alias index supports global lookups; unique indexes support scope checks. Keep; low write volume. | schema |
| M20 | `queue_timeout` is nanoseconds with no upper bound. | App validates a sane max (24h) and non-negative; DB check non-negative. | app/docs |

## Low findings

| ID | Finding | Resolution |
|---|---|---|
| L1 | `secret_enc` naming is terse. | Documented as ciphertext+tag; API never exposes it. |
| L2 | `provider_keys.scope_owner` sentinel zero UUID is unusual. | Documented; real UUIDs random; generated column prevents user input. |
| L3 | `users.created_by` self-FK can be null for bootstrap. | Intended; bootstrap owner has no creator. |
| L4 | `sessions.last_seen_idx` adds write amplification. | Needed for idle cleanup; `last_seen_at` update is rate-limited to once/minute. |
| L5 | `login_attempts` has an identity PK but is not used as a cursor. | Keeps the table vacuum-friendly; indexes cover the lockout queries. |
| L6 | `audit_log.details` 8 KiB limit uses `length(text)`. | Adequate for metadata; app validates before insert. |
| L7 | `providers.name`/`provider_keys.name` allow Unicode. | Display-only; IDs are slugs; API trims and length-checks. |
| L8 | `base_url` can contain a query string. | Allowed for non-secret routing parameters; credential-like query keys rejected by API. |
| L9 | `proxy_url` supports `socks5h` but existing script uses `socks5`. | Both allowed; no behavior change. |
| L10 | `tls` has only `default|chrome`. | Matches current `TLSProfile`; new profiles require a migration. |

## Final schema review

### Table inventory

| Table | PK | Key FKs | Cascades | Purpose |
|---|---|---|---|---|
| `users` | `id` | `created_by -> users.id` | `SET NULL` | identity/roles |
| `sessions` | `token_hash` | `user_id -> users.id` | `CASCADE` | browser sessions |
| `api_tokens` | `id` | `user_id -> users.id` | `CASCADE` | gateway/admin tokens |
| `login_attempts` | `id` | none | none | lockout evidence |
| `audit_log` | `id` | none (denormalised subjects) | none | append-only audit |
| `providers` | `id` | none | none | provider definitions |
| `provider_keys` | `id` | `provider_id -> providers.id`, `owner_id -> users.id` | `CASCADE` | credentials/scope |
| `key_models` | `(key_id, alias)` | composite `(key_id, provider_id, scope_owner) -> provider_keys` | `CASCADE` | model mappings |
| `model_state` | `model_id` | `updated_by -> users.id` | `SET NULL` | global hidden/deleted |
| `settings` | `key` | `updated_by -> users.id` | `SET NULL` | automode/cycle/search |
| `system_state` | `key` | none | none | v2 import marker |
| `config_state` | `id` | none | none | singleton routing version |

### Constraint/API agreement

| Requirement | DB enforcement | API behavior | Verified |
|---|---|---|---|
| Enabled non-none key has one credential | `num_nonnulls(...) = 1` CHECK | 422 before insert/update | yes |
| Disabled key may have no credential | `NOT enabled` branch | `clear_secret` + `enabled=false` | yes |
| Shared key only `none` | `owner_id IS NULL OR auth_scheme='none'` | shared creation restricted | yes |
| Header scheme has header name | `auth_header_name` CHECK | 422 field error | yes |
| Basic secret is JSON | app validation + decrypt | 422 on save/use | yes |
| Key/model scope cannot drift | generated `scope_owner` + composite FK + triggers | 409/422 on update | yes |
| Alias uniqueness per provider/owner | partial unique indexes | `field_errors.alias` | yes |
| Last owner survives | trigger + advisory lock | 409 | yes |
| Config writes bump version | statement triggers | no manual call | yes |
| Audit is append-only | app role grants | no update/delete route | yes |
| Secrets never returned | no SELECT projection of secret columns | `has_secret`/`last4` only | yes |
| Stats are user-filtered | JSONL subject fields | middleware filters | yes (design) |

### Trigger review

| Trigger | Table/event | Purpose | Recursion risk |
|---|---|---|---|
| `set_updated_at` | mutable tables, BEFORE UPDATE | timestamps | none |
| `provider_keys_scope_immutable_bu` | `provider_keys`, BEFORE UPDATE | block owner/provider changes | none |
| `key_models_sync_scope_bi/bu` | `key_models`, BEFORE INSERT/UPDATE | derive/reject scope | none |
| `users_guard_last_owner_bu/bd` | `users`, BEFORE UPDATE OF role,status / DELETE | last owner | none |
| `*_bump_config` | config tables, AFTER STATEMENT | version + NOTIFY | `config_state` is not triggered |
| `users_bump_config_on_delete` | `users`, AFTER DELETE | user deletion changes routing | none |

### Index review

- `sessions_user_id_idx`, `sessions_expires_at_idx`, `sessions_last_seen_idx`:
  lookup, expiry cleanup, idle cleanup.
- `api_tokens_user_id_idx`, `api_tokens_expires_at_idx`: list/revoke/expiry.
- `login_attempts_username_ts_idx`, `login_attempts_ip_ts_idx`: lockout.
- `audit_log_ts_id_idx`: cursor; `audit_log_actor/user/key/provider_idx`:
  filtered audit.
- `provider_keys_owner_name_uq` / `shared_name_uq`: duplicate names.
- `provider_keys_owner_enabled_idx`: user key lists.
- `key_models_owned_alias_uq` / `shared_alias_uq`: alias uniqueness.
- `key_models_alias_idx`, `key_models_owner_alias_idx`: alias lookups.
- No per-request routing index is required: the snapshot is loaded once per
  version, not queried per request. Adding a "routing index" would be
  over-engineering.

### Schema review result

**PASS with the revisions in the spec.** Every table and column referenced by
an API operation exists; every FK points at a primary/unique key; every
credential CHECK agrees with the key CRUD semantics; cascades agree with the
audit-before-delete rule; ownership is DB-enforced; config writes invalidate
snapshots; secrets cannot be returned; migration/bootstrap are concurrency-safe;
and phase 1 does not depend on a phase-2 job for correctness.

Two implementation notes:

1. The generated `scope_owner` columns and `UNIQUE (id, provider_id,
   scope_owner)` are deliberate. Do not replace them with a nullable composite
   FK; `MATCH SIMPLE` would skip shared keys and `MATCH FULL` would reject
   partially-null keys.
2. User-management transactions must use `READ COMMITTED` for the last-owner
   trigger to see committed demotions. Do not wrap them in `REPEATABLE READ`.

## Traceability matrix

Test coverage is **required/planned**; no test code exists in this repository
yet. Each row names the required test and the implementation issue.

| # | Requirement | DB object | API behavior | Required test coverage |
|---|---|---|---|---|
| R1 | Owner/admin/user roles; no viewer | `users.role` CHECK | role matrix, 404 cross-user | T-ROLE: matrix + IDOR (#190/#191) |
| R2 | No default password; Argon2id | `users.password_hash` | bootstrap prompts; reset flow | T-PASS: vectors/rehash/pepper (#190) |
| R3 | Last active owner cannot be removed | `users_guard_last_owner` | 409 | T-OWNER: two concurrent demotions (#190) |
| R4 | Sessions idle 12h / absolute 7d | `sessions.expires_at`, `last_seen_at` | cookie/login/logout | T-SESSION: expiry/revoke (#191) |
| R5 | Forced password change blocks access | `users.must_change_password` | 403 for sessions/tokens | T-FORCE: route matrix (#191) |
| R6 | Gateway token 365d/no-expiry; admin 90d | `api_tokens.expires_at` | create/list/revoke | T-TOKEN: lifecycle (#191) |
| R7 | Token kind/scope separation | `api_tokens.kind/scope` CHECK | admin vs gateway 401 | T-SCOPE: kind/scope (#191) |
| R8 | Personal keys only used by owner | `provider_keys.owner_id`, `scope_owner` | 404 cross-user | T-IDOR: keys (#203) |
| R9 | Shared keys only `none` | `provider_keys` CHECK | shared creation restricted | T-SHARED (#203) |
| R10 | Secrets AES-GCM at rest | `secret_enc/nonce/kver` | write-only | T-CRYPTO: round-trip/AAD (#196) |
| R11 | Exactly one credential source | `num_nonnulls` CHECK | 422 on transitions | T-CRED: transition matrix (#203) |
| R12 | Clear secret while disabling | `NOT enabled` branch | `clear_secret` + disable | T-CLEAR (#203) |
| R13 | Key/model scope cannot drift | composite FK/triggers | 409/422 | T-SCOPE (#193) |
| R14 | Provider CRUD owner/admin only | `providers` | 403/404 | T-PROVIDER (#187) |
| R15 | Alias uniqueness per provider/owner | partial unique indexes | `field_errors.alias` | T-ALIAS (#204) |
| R16 | Exact/wildcard routing | `key_models.alias` | deterministic route | T-ROUTE (#194) |
| R17 | Fallback statuses/loops | `fallbacks`, `fallback_statuses` | fallback chain | T-FALLBACK (#195) |
| R18 | Hidden/deleted not routable | `model_state`, `hidden` | 404 | T-HIDDEN (#201) |
| R19 | Every config write invalidates snapshot | `config_state` + triggers | no stale route | T-VERSION (#185/#186) |
| R20 | Consistent snapshot | `REPEATABLE READ` load | atomic publish | T-SNAPSHOT (#186) |
| R21 | DB outage keeps last snapshot | snapshot/health | degraded, 503 writes | T-DOWN (#186) |
| R22 | Audit append-only, subject columns | `audit_log` | owner cursor read | T-AUDIT (#191/#196) |
| R23 | Stats user-filtered, no secrets | JSONL additive fields | own/all by role | T-STATS (#191) |
| R24 | v2 provider compatibility | `LegacyView` | mapping/removed fields | T-V2 (#187) |
| R25 | Migration lock/schema guard | `schema_migrations`, `config_state` | startup refusal | T-MIGRATE (#185) |
| R26 | Bootstrap concurrency | `users` + advisory lock | first owner only | T-BOOT (#186) |
| R27 | SSRF/sensitive config rejected | app validation | 422 | T-SSRF (#187) |
| R28 | Audit/redaction | `audit_log.details` | no secret in API/logs | T-REDACT (#200) |
| R29 | v2 import round-trip | all config tables | `LegacyView` equality | T-IMPORT (#187) |
| R30 | Dev DB readiness/token | compose/run.ps1 | clear startup errors | T-DEV (#212) |

## Readiness assessment

**READY WITH DOCUMENTED FOLLOW-UPS.**

Blocking findings: none. Critical (C1–C10) and high (H1–H10) findings are
resolved in the revised spec. The final schema review passes. Phase 1 has no
dependency on a phase-2 job for authentication, authorization, secret handling,
routing correctness, snapshot consistency or last-owner safety.

Documented follow-ups, all non-blocking for the first cutover:

1. **Cross-instance stats aggregation** — JSONL is per process. If the product
   needs one global stats view across instances, add a PostgreSQL
   `request_events` table in phase 2. Phase 1 correctly filters by user on the
   serving instance and does not pretend to aggregate.
2. **Audit retention** — audit grows until the phase-2 maintenance job. Admin
   action volume is low; the app role cannot delete audit rows.
3. **Master-key rotation** — phase 2; the envelope already carries
   `secret_kver` and the AAD binds it, so rotation can be added without a schema
   change.
4. **Per-token/per-IP rate limits** — phase 2; login lockout and token expiry
   exist in phase 1.
5. **Backup restore drill and audit tamper tests** — phase 2 hardening.

What must happen during implementation:

- Create the migration with the exact checks/triggers in the spec; do not omit
  the generated `scope_owner` or the statement triggers.
- Add the required test suites in §13; the traceability matrix is the checklist.
- Keep the existing three-file embed contract and the existing provider/model
  routes.
- Extend the existing `docker-compose.yml`; do not create a second compose file.
- Replace `scripts/run.ps1`'s hardcoded `local-gateway` with
  `GNVIPI_GATEWAY_TOKEN` (#212).

This audit does not claim the implementation is ready. It claims the design is
specific enough to implement without guessing about ownership, constraints,
transaction semantics, routing behavior or secret handling.