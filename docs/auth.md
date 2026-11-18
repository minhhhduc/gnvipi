# Authentication, roles and keys

How ownership, sign-in, bootstrap, recovery and provider credentials work in
database mode. Database mode is enabled by `GNVIPI_DATABASE_URL`; without it the
gateway keeps the file-backed admin token and `-gateway-key` behaviour, where
roles do not exist.

Operational steps for PostgreSQL itself are in
[postgres-operations.md](postgres-operations.md). Provider schema reference is in
[providers.md](providers.md). Run flags are in [operations.md](operations.md).

## Ownership model

- A **user** owns an account, its sessions and API tokens, and any provider keys
  created with that user as owner.
- A **provider** is global configuration. Its keys are rows in `provider_keys`,
  not fields on the provider.
- A **personal key** has `owner_id` set. Only that user may route through it.
- A **shared key** has `owner_id` NULL. Shared keys must use `auth_scheme =
  none`, so a shared key never carries another user's credential. Only a
  superuser may create one.
- Routing never reads another user's key. If an alias exists but the caller has
  no usable key, the request returns 403; an unknown alias returns 404; an
  unresolvable credential returns 503.
- Deleting a user cascades their sessions and tokens and deletes their personal
  keys, with an audit entry. Shared keys are unaffected.
- The last active superuser cannot be deleted, disabled or demoted. A database
  trigger is the final authority, so the rule also holds outside the console.
  Root counts as a superuser-level account here, so promoting a superuser to
  root keeps the guard satisfied.
- The built-in **root** account (`gnvipi`) sits above superuser. It is seeded on
  every database-mode start and the database refuses to delete, rename, demote
  or disable it, so a console mistake or direct `DELETE` cannot remove the
  recovery identity. Root is not assignable: the admin API rejects
  `role: "root"` on create and update, and the only path that creates it is
  `auth.EnsureRoot` at startup.

### The built-in root account

Every database-mode start ensures the account `gnvipi` exists:

- If `GNVIPI_ROOT_PASSWORD` is set, the account is created with that password
  and no forced change. That variable is the way to own the password from the
  environment.
- Otherwise the account is created with the documented default password
  `gnvipi` and `must_change_password`, so the first sign-in must choose a real
  password before any other admin route answers. The startup log warns while
  the default is still in use. The login policy refuses a password containing
  the username, so only the seed can ever set that default.
- An account named `gnvipi` that already exists with another role is promoted
  to root, keeping its password. An existing root row is never rewritten, so a
  password set in the console survives restarts.
- Seeding and promotion are audited as `user.root.create` and
  `user.root.promote`.

The server returns `403 {"code":"root_protected"}` for any attempt to delete,
rename, demote or disable the account, and the same holds in `psql`
(`root account cannot be deleted`).

### Key selection precedence

For one authenticated user and one requested alias, candidates are tried in this
order:

1. Personal exact alias.
2. Personal wildcard alias, longest prefix first.
3. Shared `none` exact alias.
4. Shared `none` wildcard alias.

Ties use the key's configured position, then provider ID and key ID. Disabled
providers and keys are excluded. Fallback expansion stops at 8 candidates;
fallbacks resolve with the same user, so a fallback can never cross into another
account's key.

### Shared key model ids

A shared key belongs to the operator, so its mappings are not the caller's
configuration to read. A caller selects a shared exact mapping by an id derived
from the provider and the alias:

```
id = base32(sha256(provider_id + NUL + alias))[0:26], lowercase
```

The console (provider view and playground picker) shows that id in place of the
alias and omits the upstream model and the fallbacks; a request naming the id
routes to the shared mapping, and the id, not the alias, is echoed back in
`model` and in stats. Personal mappings keep their alias: their owner supplies
the credential, so nothing there is the operator's to hide. Alias uniqueness
stays per provider, and the provider ID is part of the derivation, so the same
alias on two providers yields two different ids.

The id is stable across restarts and instances because it is derived, never
stored. Consequences:

- A caller can address a shared mapping without the operator's alias being
  advertised, and the alias is not returned by any console or inference response.
- A shared mapping hidden behind `hidden`, a `model_state` hide, or a retired
  mapping returns 404 by id exactly as it does by alias.
- Once the account has a personal mapping for the same alias, the shared id
  stops resolving: personal precedence is the same rule for ids and aliases.
- Wildcard patterns (`vendor/*`) stay patterns, not ids: a pattern has no single
  upstream model to derive an id from, so only exact shared mappings are
  addressed this way.

## Roles

Root holds every capability of a superuser, on every route, and cannot be
demoted. That includes the account selectors on the playground and config
pages, where root may name any user id exactly as a superuser may. The table
lists what superuser, admin and user reach.

| Capability | Superuser | Admin | User |
|---|---|---|---|
| Inference through own gateway token | yes | yes | yes |
| Own sessions, password, gateway tokens | yes | yes | yes |
| See own user record; reset own password | yes | yes | yes |
| List provider metadata | yes | yes | yes |
| List provider keys | all | all | own only |
| Create/update/delete own personal keys | yes | yes | yes |
| Create keys for another user | yes | no | no |
| Create shared `none` keys | yes | no | no |
| Write `key_ref` keys | yes | own keys | no |
| Create/update/delete providers | yes | yes | no |
| Models and stats | yes | yes | yes |
| Own automode/cycle settings | yes | yes | yes |
| Global automode/cycle and web search config (file mode) | yes | yes | no |
| Chrome pools | yes | yes | no |
| List/create/update/delete users | yes | no | no |
| Read audit log | yes | no | no |
| Mint tokens for another user | yes | no | no |

Notes:

- Every signed-in role polls stats after any mandatory password change. User
  accounts see only their own request events on Dashboard and Usage log,
  including historical frames; global model and traffic aggregates stay hidden.
- An admin can read every provider key's metadata but cannot read a secret. On a
  key owned by someone else, or a shared key, an admin may only disable it; the
  name, scheme, credential and deletion stay with the owner or the key's user.
- Shared keys are created and fully managed by a superuser. No user owns them.
- A user may not hold an admin token. Admin tokens require a `read` or `write`
  scope and an expiry; gateway tokens have no scope and may be non-expiring.
- `must_change_password` blocks every admin API route except login, logout,
  session lookup and password change, regardless of role.
- Root is a superuser for every declared route, so it also reaches the screens
  listed as superuser-only. The console still applies the same CSRF and
  `must_change_password` gates, and its Users row has no role select, disable or
  delete control.
- File mode is different: one `-admin-token` protects `/admin`, and
  `-gateway-key` protects inference routes. There are no users or roles.

Browser sessions use HttpOnly, SameSite-strict cookies. The idle timeout is 12
hours and the absolute lifetime is 7 days. Plain-HTTP cookie login is accepted
only on a loopback bind; remote browser login requires TLS. All mutations
require same-origin browser provenance.

## Bootstrap

Prerequisites: PostgreSQL is running, schema is migrated and
`GNVIPI_MASTER_KEY` is set.

```powershell
docker compose up -d --wait db
$env:GNVIPI_DATABASE_URL='postgres://gnvipi_dev:gnvipi_dev_only@localhost:5432/gnvipi?sslmode=disable'
$env:GNVIPI_MASTER_KEY='<base64-32-byte-key>'
go build -o .\bin\serve.exe ./cmd/serve
.\bin\serve.exe migrate

'<temporary-password>' | .\bin\serve.exe admin create-superuser `
  --username <superuser> --email <superuser@example.com> --display-name '<Superuser>' `
  --with-gateway-token
```

`create-superuser` flags are `--username` (required), `--email`, `--display-name`
and `--with-gateway-token`. Usernames are case-insensitive and 3 to 64
characters. The password is read from stdin, never from a flag, and is never
written to output or audit.

The command refuses a terminal stdin so the password is not echoed. On Windows
it also refuses an MSYS2/mintty pipe, because that pseudo-terminal is a named
pipe and cannot be told apart from a real pipeline. Pipe from cmd.exe or
PowerShell instead.

The superuser and the optional gateway token are created in one transaction.
Nothing is printed before the commit. The token is printed once, with the
`gw_` prefix, and the plaintext is not stored. Save it in a password manager
before closing the terminal. If stdout fails after the commit, the error names
the superuser that exists and the recovery command.

Example output:

```text
superuser "<superuser>" created (id <uuid>)
gateway token (shown once): gw_...
```

### One-time `GNVIPI_ADMIN_TOKEN`

`GNVIPI_ADMIN_TOKEN` (or `-admin-token`) has two different jobs:

- In file mode it is the normal admin token.
- In database mode it is a **bootstrap** token. It authenticates only while the
  `users` table is empty, and only to create the first superuser through the
  admin API. It cannot create a second user, a non-superuser, or read any other
  route.
  Once any user exists it is ignored, even if still set. It never authenticates
  `/v1` requests.

The console route is `POST /admin/api/users` with the bootstrap bearer token and
a body of `{"username":"<superuser>","password":"<password>","role":"superuser"}`.
The `serve admin create-superuser` command is usually simpler.

An explicit `-admin-token` value overrides `GNVIPI_ADMIN_TOKEN`, including an
explicit empty value, which disables bootstrap. On a non-loopback bind, prefer
TLS and send the bootstrap token as a bearer token.

### Importing a v2 file

Existing schema-v2 files can be imported into the database:

```powershell
.\bin\serve.exe admin import-v2 --owner <owner> --file .\path\to\models.json
```

- `--owner` is required and must name an existing user.
- `--file` defaults to `internal/models/playground_models.json`.
- `--force` replaces a previous import from a different source.
- Credentialed v2 keys are attached to `--owner`.
- A v2 `key_ref` is resolved from the adjacent `secrets.json` during import and
  stored as ciphertext, so imported database keys are self-contained.
- Import requires `GNVIPI_MASTER_KEY`.

## Recovery

### Lost password or locked account

Run the reset command with the runtime DSN. It uses the same schema check as the
server and does not need another signed-in user:

```powershell
'<temporary-password>' | .\bin\serve.exe admin reset-password <username>
```

The reset:

- sets a temporary password and sets `must_change_password`;
- clears failed attempts and any lockout;
- revokes every session and every API token for that user;
- writes an audit entry naming the system as actor.

The user signs in with the temporary password and changes it. Because the reset
revokes gateway tokens, mint a new token afterwards if the account still needs
programmatic access.

If a superuser is available, the console can perform the same reset on the Users
page. The command is the fallback when no session can be created.

### Lost gateway token

Sign in to the console and create a new gateway token for the account. The token
is shown once. A superuser can create a gateway token for any user; admin and user
accounts can only create tokens for themselves. Resetting the password also
revokes the old token, but it is not required just to replace one.

### Lost `GNVIPI_ADMIN_TOKEN`

After the first superuser exists the bootstrap token is ignored, so losing it has no
effect. If the database is still empty and the token is lost, set a new
`GNVIPI_ADMIN_TOKEN` before creating the first superuser, or use
`serve admin create-superuser`.

### Lost master key

Stored provider secrets cannot be decrypted without the master key that
encrypted them. There is no recovery path and no fallback key. Environment
credentials (`key_env`) are unaffected. Re-enter secrets for each affected key.

Back up the master key separately from database backups. A database restore
with a different key produces providers whose credentials cannot be decrypted,
so their requests return 503.

## Provider secrets

Provider credentials are write-only. The admin API returns metadata, never the
secret:

| Field | Meaning |
|---|---|
| `has_secret` | A stored secret exists for this key. |
| `secret_last4` | Last four display characters computed at write time; empty when there is no stored secret. |

Stored secrets are encrypted with AES-256-GCM before insertion. The 12-byte
nonce, ciphertext and key version are separate columns. The additional
authenticated data binds the key ID, owner scope, auth scheme and key version,
so moving a ciphertext to another key, owner, scheme or version does not
decrypt. There is no plaintext secret column and no read-back endpoint.

### Credential sources

Each enabled, non-`none` key has **exactly one** source. The database enforces
this; there is no precedence and no fallback between sources.

| Source | Column | How the value is obtained |
|---|---|---|
| Stored secret | `secret_enc` (+ `secret_nonce`, `secret_kver`) | Decrypted at request time with the keyring. Encrypted at write time from console or `import-v2`. |
| Environment | `key_env` | Read from the gateway process environment at request time. |
| Reference | `key_ref` | Legacy only. Converted into a stored secret at startup from the `secrets.json` beside the configured models file; the admin API rejects new `key_ref` writes. |

Rules verified in the schema and resolver:

- Two sources on one key are rejected (`provider_keys_credential_single`).
- `auth_scheme = none` must have zero sources.
- A disabled key may have zero sources; `clear_secret` with `enabled = false` is
  a legal state.
- `clear_secret` and a new secret are mutually exclusive.
- Changing `auth_scheme` while keeping a stored secret is rejected; re-enter
  the secret.
- `key_env` is restricted to the `GNVIPI_KEY_*` namespace. `GNVIPI_MASTER_KEY`
  and other names are rejected.
- The admin API refuses a new `key_ref` (422) for every role, the superuser
  included: a reference points into the adjacent `secrets.json`, which the
  gateway reads once at startup and never again. Store the secret value instead.
- `key_ref` rows that already exist are materialized into a stored secret at
  startup, before routing is published. A reference missing from the file (or a
  gateway without `GNVIPI_MASTER_KEY`) leaves the row untouched, is logged by key
  id, and keeps returning the credential-unavailable 503 rather than falling
  back.

Basic auth secrets are JSON objects with `username` and `password`. Bearer and
header secrets are the raw token value.

### Root account and master key

| Variable | Meaning |
|---|---|
| `GNVIPI_ROOT_PASSWORD` | Password for the built-in root account at first seed. Without it the account uses the default `gnvipi` and must change it at sign-in. |
| `GNVIPI_MASTER_KEY` | Base64-encoded 32-byte key. Required in database mode. |
| `GNVIPI_MASTER_KEY_VERSION` | Positive integer version; defaults to `1`. Encryption always uses this version. |
| `GNVIPI_MASTER_KEY_PREVIOUS` | Comma-separated `version:base64` pairs accepted for decryption. |

Database mode fails at startup when the master key is missing or malformed.
Every gateway instance pointing at the same database must use the same key, and
a restore is only complete when the matching key is available.

The keyring can hold previous versions and still decrypt their rows. It does not
re-encrypt existing rows.

> [!IMPORTANT]
> **Phase-2 rotation is not implemented.** There is no rotate or re-encrypt
> command in this release. `GNVIPI_MASTER_KEY_PREVIOUS` is plumbing for a staged
> rotation, not a finished procedure: adding a new current key leaves old rows
> readable but never rewrites them, so the old key cannot be retired. Do not
> rotate the master key in place until the phase-2 workflow ships. Back up the
> current key separately and treat losing it as losing every stored secret.

Password hashing uses its own secrets, configured independently:
`GNVIPI_PASSWORD_PEPPER` and `GNVIPI_PASSWORD_PEPPER_PREVIOUS`, plus the
`GNVIPI_PASSWORD_ARGON2_*` tuning variables. They are not part of the provider
keyring.

## Per-user automode and cycle

With `GNVIPI_DATABASE_URL` set, `automode` and `cycle` are **per account**:

- They live in `user_routing_prefs`, one row per user, deleted with the user.
- `GET`/`PUT /admin/api/config/automode` and `/cycle` act as the signed-in
  account. A superuser may pass `?user_id=<uuid>` to read or edit another
  account; anyone else naming a foreign id gets the same 404 as a missing one.
- Resolution order is the account's own setting, then the global default, then
  a typed "not configured" error. The chosen target must resolve through the
  account's own provider keys plus shared `none` keys; a target reachable only
  through another account's key is rejected on write and on resolve.
- The cycle cursor is per account **and per process**: restarting the gateway
  resets each account's rotation position.
- Model lists are validated on write; an unknown alias returns
  `field_errors.models`. Each write is one transaction and is audited as
  `routing.automode.update` or `routing.cycle.update` with the model list.
- File-backed mode (no `GNVIPI_DATABASE_URL`) keeps the single global
  behaviour, edited by superuser and admin accounts.

## `scripts/run.ps1 -Claude`

`-Claude` starts the gateway in the background, waits for `/healthz`, points
Claude Code at it, and stops the gateway when Claude exits. It requires a real
gateway token:

```powershell
$env:GNVIPI_GATEWAY_TOKEN='gw_...'
.\scripts\run.ps1 -Claude
```

- The token must come from the database (bootstrap output or a console-created
  gateway token).
- The script rejects a missing token and rejects the literal `local-gateway`.
  That literal is only a legacy `-gateway-key` placeholder; it is not a database
  credential.
- The script sets `ANTHROPIC_BASE_URL` to the gateway root,
  `ANTHROPIC_AUTH_TOKEN` to the gateway token and clears `ANTHROPIC_API_KEY`.
  It restores the caller's environment on exit.
- The token is forwarded to the Claude Code child process and is never printed.
- When `GNVIPI_DATABASE_URL` or `GNVIPI_MASTER_KEY` is set, the script first
  checks that PostgreSQL is reachable, using `psql` or the matching local
  compose service, and refuses to start otherwise.

To create the token:

```powershell
.\bin\serve.exe admin create-superuser --username <superuser> --with-gateway-token
```

or sign in to `/admin` and create a gateway token on the account page.

## Not implemented

The following are explicitly outside this release. Do not document or script
around them as if they exist:

- Automatic provider key rotation or failover between credentials on 401, 403
  or quota errors. Model fallback chains exist; credential failover does not.
- Quotas or spend/cost limits per user, key or provider.
- Credit, RPM or TPM budgets or throttles.
- Per-token rate limits.
- A master-key rotation or re-encrypt command (phase 2).
- Reading a stored provider secret back through any API.
- Shared keys that carry a credential; shared keys are `none` only.
- Per-user provider definitions; providers are global.