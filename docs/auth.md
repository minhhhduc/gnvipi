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
  none`, so a shared key never carries another user's credential. Only an owner
  may create one.
- Routing never reads another user's key. If an alias exists but the caller has
  no usable key, the request returns 403; an unknown alias returns 404; an
  unresolvable credential returns 503.
- Deleting a user cascades their sessions and tokens and deletes their personal
  keys, with an audit entry. Shared keys are unaffected.
- The last active owner cannot be deleted, disabled or demoted. A database
  trigger is the final authority, so the rule also holds outside the console.

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

## Roles

| Capability | Owner | Admin | User |
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
| Models, stats, automode/cycle/search config, Chrome pools | yes | yes | no |
| List/create/update/delete users | yes | no | no |
| Read audit log | yes | no | no |
| Mint tokens for another user | yes | no | no |

Notes:

- An admin can read every provider key's metadata but cannot read a secret. On a
  key owned by someone else, or a shared key, an admin may only disable it; the
  name, scheme, credential and deletion stay with the owner or the key's user.
- Shared keys are created and fully managed by the owner. No user owns them.
- A user may not hold an admin token. Admin tokens require a `read` or `write`
  scope and an expiry; gateway tokens have no scope and may be non-expiring.
- `must_change_password` blocks every admin API route except login, logout,
  session lookup and password change, regardless of role.
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

'<temporary-password>' | .\bin\serve.exe admin create-owner `
  --username <owner> --email <owner@example.com> --display-name '<Owner>' `
  --with-gateway-token
```

`create-owner` flags are `--username` (required), `--email`, `--display-name`
and `--with-gateway-token`. Usernames are case-insensitive and 3 to 64
characters. The password is read from stdin, never from a flag, and is never
written to output or audit.

The command refuses a terminal stdin so the password is not echoed. On Windows
it also refuses an MSYS2/mintty pipe, because that pseudo-terminal is a named
pipe and cannot be told apart from a real pipeline. Pipe from cmd.exe or
PowerShell instead.

The owner and the optional gateway token are created in one transaction.
Nothing is printed before the commit. The token is printed once, with the
`gw_` prefix, and the plaintext is not stored. Save it in a password manager
before closing the terminal. If stdout fails after the commit, the error names
the owner that exists and the recovery command.

Example output:

```text
owner "<owner>" created (id <uuid>)
gateway token (shown once): gw_...
```

### One-time `GNVIPI_ADMIN_TOKEN`

`GNVIPI_ADMIN_TOKEN` (or `-admin-token`) has two different jobs:

- In file mode it is the normal admin token.
- In database mode it is a **bootstrap** token. It authenticates only while the
  `users` table is empty, and only to create the first owner through the admin
  API. It cannot create a second user, a non-owner, or read any other route.
  Once any user exists it is ignored, even if still set. It never authenticates
  `/v1` requests.

The console route is `POST /admin/api/users` with the bootstrap bearer token and
a body of `{"username":"<owner>","password":"<password>","role":"owner"}`. The
`serve admin create-owner` command is usually simpler.

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

If an owner is available, the console can perform the same reset on the Users
page. The command is the fallback when no session can be created.

### Lost gateway token

Sign in to the console and create a new gateway token for the account. The token
is shown once. An owner can create a gateway token for any user; admin and user
accounts can only create tokens for themselves. Resetting the password also
revokes the old token, but it is not required just to replace one.

### Lost `GNVIPI_ADMIN_TOKEN`

After the first owner exists the bootstrap token is ignored, so losing it has no
effect. If the database is still empty and the token is lost, set a new
`GNVIPI_ADMIN_TOKEN` before creating the first owner, or use
`serve admin create-owner`.

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
| Reference | `key_ref` | Resolved at runtime from the `secrets.json` beside the configured models file. |

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
- `key_ref` writes require owner or admin. A `key_ref` created this way is
  resolved against the adjacent secrets file; a missing file means those keys
  return a credential-unavailable 503 rather than falling back.

Basic auth secrets are JSON objects with `username` and `password`. Bearer and
header secrets are the raw token value.

### Master key

| Variable | Meaning |
|---|---|
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

In this release `automode` and `cycle` are **global** settings. They live in the
single `settings` row and every account resolves them through its own keys.
Only owner and admin accounts may read or change them. File-backed mode keeps
the same single global behaviour.

Per-user routing preferences are not implemented yet. Issue #240 proposes a
`user_routing_prefs` table, per-caller `automode`/`cycle` endpoints, a per-user
cycle cursor, and owner-only access to another account's settings. Until that
ships, there is no per-user target, no per-user cycle list and no way for a
regular user to read or write these settings. Do not provision around a
per-user table that does not exist.

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
.\bin\serve.exe admin create-owner --username <owner> --with-gateway-token
```

or sign in to `/admin` and create a gateway token on the account page.

## Not implemented

The following are explicitly outside this release. Do not document or script
around them as if they exist:

- Automatic provider key rotation or failover between credentials on 401, 403
  or quota errors. Model fallback chains exist; credential failover does not.
- Per-user `automode` and `cycle` (issue #240).
- Quotas or spend/cost limits per user, key or provider.
- Credit, RPM or TPM budgets or throttles.
- Per-token rate limits.
- A master-key rotation or re-encrypt command (phase 2).
- Reading a stored provider secret back through any API.
- Shared keys that carry a credential; shared keys are `none` only.
- Per-user provider definitions; providers are global.