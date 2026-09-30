# Chipmank

A WhatsApp marketing platform for a single operator: connect a business's WhatsApp
Business Account, send campaign and template messages at scale, and keep every
conversation, consent decision and delivery receipt in one place with a provable
audit trail.

This repository is the implementation of that idea, starting from the one part
that has to be right before anything else matters: **inbound webhooks**.

---

## The idea

Most WhatsApp automation is a script that posts to the Cloud API. That works
until something goes wrong, and then it works *wrongly*:

- The API call succeeds, but the app crashes before it records anything.
- A webhook arrives, is verified, and is dropped by a restart mid-write.
- The same delivery arrives twice and the contact gets messaged twice.
- A retry from Meta returns an error, so Meta retries forever, forever.
- Two tenants share a database and one can read the other's customers.

Every one of these is the same underlying mistake: **acknowledging work before it
is durably recorded**. The web server replies `200 OK`, Meta believes the
delivery was consumed, and the message is gone. No log, no retry, no trace.

Chipmank is built the other way around. The `200` is the last thing that happens,
not the first:

```
Meta  ──►  verify signature  ──►  commit to Postgres  ──►  reply 200
             (exact bytes)         (durable, deduped)
             └──────────── anything earlier fails ───────────┘
```

A crash at any point before the commit produces a non-200, which makes Meta retry,
which is safe because the write is idempotent. That single ordering decision is
the foundation everything else is built on.

### What makes it trustworthy

| Guarantee | How it is enforced |
| --- | --- |
| A `200` means the bytes are on disk | Commit before responding; `503` on any append failure |
| Signatures are checked against reality | HMAC over the **exact raw bytes**, never a re-serialized struct |
| Retries never duplicate | Unique constraints on delivery key and per-item fingerprint, deduped via savepoints |
| Malformed traffic is kept, not dropped | Authenticated garbage is stored and acknowledged, so it can be investigated |
| Unauthenticated traffic never persists | Verification happens before anything touches the database |
| One tenant cannot see another | RLS plus transaction-local `app.tenant_id`; an unscoped pooled connection reads **zero rows**, not everything |
| Raw inbound bodies stay unreadable by the role that writes them | `app_ingress` holds no `SELECT` on `ingress_items` |
| Provider secrets are never stored | `credential_ref` holds a reference; the database holds no secret values |

The last three are the ones that usually get skipped and then regretted. They are
covered by 29 schema/RLS checks that specifically try to break them.

---

## How it is put together

Deliberately plain. One language per concern, one database, no distributed
infrastructure until there is a reason to add it.

```
services/ingress/     Go   webhook receiver (this is what exists today)
services/dispatch/    Go   send workers, rate limiting, retries, status reconciliation
apps/api/             TS   REST/GraphQL surface for the dashboard
apps/dashboard/       TS   Next.js operator UI
db/migrations/        SQL  schema, roles, RLS — the source of truth
proto/                     internal contracts between services
tools/dbtest/         PGlite harness: schema suite, wire server, read-back verifier
tools/verify.sh            runs every check
docs/                      architecture, roadmap, project plan
```

Why Go for services: webhook handling and send workers are I/O-bound, long-running
processes where predictable latency and trivial deployment matter more than
iteration speed. Why TypeScript for the API and dashboard: the operator surface is
forms and tables, and sharing types with the frontend is worth more than
polymorphism elsewhere.

Why PostgreSQL alone: this system's hard parts — tenant isolation, idempotency,
quota reservation, outbox delivery, status reconciliation — are all transactional
problems that PostgreSQL already solves correctly. Adding Kafka or Temporal would
mean re-implementing guarantees the database gives away, and then owning the
failure modes that come with them.

---

## Current state

The ingress slice is **done and verified**. Everything below it has not been built
yet; the plan is in [`docs/ROADMAP.md`](docs/ROADMAP.md) and
[`docs/PMD.md`](docs/PMD.md).

**Working and tested**

- 11 migrations: identity, channels, contacts, conversations, send path, status,
  ingress, outbox, roles, RLS. Extension-free, so it runs on stock PostgreSQL 13+.
- Webhook receiver: HMAC-SHA256 over raw bytes, durable pgx appender, delivery-key
  and item-level dedupe, unroutable numbers stored without a tenant.
- Simulator covering message, status, error, malformed, empty, byte-identical
  retries and tampered bodies.
- Verification: 29 schema/RLS checks, 8 pgx integration tests, unit tests for HMAC
  and HTTP. `tools/verify.sh` runs the lot.

**Not done**

- Outbox relay, dispatch worker, the send path itself, status reconciliation.
- API and dashboard.
- Real Meta Cloud API integration.

**Known limits — do not exceed these**

- There is **no real PostgreSQL** in this environment. Testing uses PGlite over the
  wire protocol.
- PGlite serializes all queries, so `FOR UPDATE SKIP LOCKED`, job lease races,
  outbox throughput and pooled-connection tenant reset are **untested**. They need
  a real server before anyone claims they work.
- There is **no Meta account**. All webhook traffic is generated locally. No real
  Cloud API call has been made.

---

## Running it

Requires Go 1.25+ and Node 18+.

**1. Database.** No PostgreSQL available here, so `tools/dbtest` provides a PGlite
server that speaks the PostgreSQL wire protocol, applies all migrations, and
seeds fixture tenants and routing bindings:

```bash
cd tools/dbtest && npm install
PGLITE_FRESH=1 node serve.mjs      # 127.0.0.1:55432
```

**2. Service.**

```bash
export META_APP_SECRET=dev-secret-abc
export INGRESS_DATABASE_URL="postgres://postgres@127.0.0.1:55432/postgres?default_query_exec_mode=exec"
export INGRESS_ADDR=:8098
go run ./services/ingress/cmd/ingress
```

With a database configured it uses the durable store. Without one it refuses to
start unless you explicitly opt into the in-memory store, which loses deliveries
on restart. **A missing database is never silently downgraded** — that failure mode
is exactly what this whole project exists to prevent.

**3. Simulator.**

```bash
go run ./services/ingress/cmd/simulator -scenario message -fixed -count 3
```

`message`, `status`, `error`, `malformed`, `empty`. `-fixed` sends a
byte-identical body to simulate a Meta retry; `-tamper` alters the body after
signing.

**4. Verify everything.**

```bash
tools/verify.sh
```

**5. Confirm what actually landed.**

```bash
cd tools/dbtest && node readback.mjs
```

`readback.mjs` opens the database independently and re-derives each stored
payload's HMAC from the bytes on disk. If a delivery was ever acknowledged without
being durably written, this is what catches it. It is the closest thing here to a
proof rather than a test.

---

## Two environment traps worth knowing

**PGlite cannot serve pgx's prepared-statement cache.** Re-parsing the same
statement name fails with `42P05 prepared statement ... already exists`, which
surfaces as confusing errors like `expected 0 arguments, got 1`. Append
`default_query_exec_mode=exec` to the DSN. Real PostgreSQL has no such limitation
and production should keep pgx's default cached mode — the parameter exists purely
to work around the emulator.

**PGlite serializes everything** through one global query queue. Any concurrency
guarantee you want to rely on has to be proven on a real server.

---

## Documents

| Document | What it covers |
| --- | --- |
| [`docs/ROADMAP.md`](docs/ROADMAP.md) | Phases, current status, what ships when |
| [`docs/PMD.md`](docs/PMD.md) | Workstreams, task breakdown, sequencing, risks, definition of done |
| `docs/Enterprise_WhatsApp_Platform_Architecture_v2.pdf` | Canonical system architecture |
| `docs/Enterprise_WhatsApp_Platform_Architecture.pdf` | Earlier architecture draft |
| `docs/Enterprise_WhatsApp_Technology_Stack.pdf` | Technology reference |
| `docs/whatsapp-platform-architecture-v2.md` | Markdown of the v2 architecture |
| `docs/whatsapp-platform-tech-stack.md` | Lean technology decisions |

The v2 PDF is canonical. The non-v2 PDF and the markdown predating it are kept for
history and contain conflicting details — prefer v2.