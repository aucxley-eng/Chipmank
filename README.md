# chipmank

WhatsApp marketing platform. Currently at the first vertical slice: **Meta webhook
ingress**, durable in PostgreSQL, driven by a local simulator in place of the real
Meta Cloud API.

## Status

| Piece | State |
| --- | --- |
| Schema + RLS + roles (11 migrations) | done, 29/29 isolation checks pass |
| HMAC verification over raw bytes | done |
| Durable pgx appender (dedupe, routing, byte-exact) | done, 8/8 integration tests pass |
| Simulator -> service -> Postgres -> independent readback | done, verified |
| Outbox relay / dispatch worker | not started |
| Real Meta Cloud API | blocked, needs app + WABA + test number |

## Layout

```
db/migrations/          0001..0011, extension-free, roles before RLS
services/ingress/       Go webhook receiver
  cmd/ingress/          server entrypoint
  cmd/simulator/        signed webhook generator
  internal/verify/      HMAC
  internal/httpapi/     HTTP contract and status codes
  internal/metaevent/   envelope parsing, delivery/item fingerprints
  internal/store/       memory + postgres appenders
tools/dbtest/           PGlite schema suite, wire server, readback
```

## Running it

Requires Go 1.25+ and Node 18+.

### 1. Database

There is no real PostgreSQL available in this environment, so `tools/dbtest`
provides a PGlite server that speaks the PostgreSQL wire protocol:

```bash
cd tools/dbtest && npm install
PGLITE_FRESH=1 node serve.mjs      # listens on 127.0.0.1:55432
```

This applies all 11 migrations and seeds fixture tenants, channel accounts and
routing bindings.

### 2. Service

```bash
export META_APP_SECRET=dev-secret-abc
export INGRESS_DATABASE_URL="postgres://postgres@127.0.0.1:55432/postgres?default_query_exec_mode=exec"
export INGRESS_ADDR=:8098
go run ./services/ingress/cmd/ingress
```

With `INGRESS_DATABASE_URL` set the service uses the durable store. Without it,
it refuses to start unless you also set `INGRESS_ALLOW_EPHEMERAL_STORE=1`, which
acknowledges webhooks into memory and loses them on restart. A missing database is
never silently downgraded.

### 3. Simulator

```bash
go run ./services/ingress/cmd/simulator -scenario message -fixed -count 3
```

Scenarios: `message`, `status`, `error`, `malformed`, `empty`. `-fixed` sends a
byte-identical body (retry simulation), `-tamper` alters the body after signing.

## Verifying

```bash
# unit + integration + schema/RLS
bash tools/verify.sh

# what actually landed, read from disk by an independent process
cd tools/dbtest && node readback.mjs
```

`readback.mjs` re-derives each stored payload's HMAC from the bytes on disk. If a
delivery ever gets acknowledged without being durably written, this is what
catches it.

## Two things that will waste your time if you don't know them

**PGlite cannot serve pgx's prepared-statement cache.** Re-parsing the same
statement name fails with `42P05 prepared statement ... already exists`. Append
`default_query_exec_mode=exec` to the DSN. Real PostgreSQL has no such limitation
and production should keep the default cached mode; the parameter only exists to
work around the emulator.

**PGlite serializes everything.** The socket server has one global query queue, so
`FOR UPDATE SKIP LOCKED`, job lease races, outbox throughput and pooled-connection
tenant reset are all untested here. The schema suite says so at the end of its
output. Those need a real server.

## Security properties this design depends on

- Signatures are checked against the **exact** raw bytes before anything is parsed
  or stored. Nothing unauthenticated reaches the database.
- `app_ingress` can insert ingress rows but has **no** `SELECT` on
  `ingress_items`; it can only read routing bindings. Raw inbound bodies are not
  readable by the role that writes them.
- Tenant isolation is RLS plus a transaction-local `app.tenant_id`. An unscoped
  pooled connection sees zero rows rather than everything.
- Secrets live behind `credential_ref`; the database stores references, not
  provider secrets.