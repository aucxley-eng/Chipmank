# Project Management Document

How the build is organised: workstreams, task breakdown, sequencing rules,
risks, and what "done" means here.

Complements [`ROADMAP.md`](ROADMAP.md). The roadmap answers **what** ships and in
what order; this document answers **how** the work is decomposed and judged.

---

## Operating principles

1. **Durability before features.** No user-facing capability ships until its
   failure mode is understood and tested.
2. **Prove it or mark it unproven.** If the environment cannot exercise a
   guarantee, it gets written as unverified. It never gets quietly assumed.
3. **Refuse to degrade.** A missing dependency should stop the process loudly, not
   silently substitute something that loses data.
4. **Boring technology.** PostgreSQL and Go, until measured evidence says
   otherwise.
5. **One operator.** No on-call rotation, no distributed systems for hypothetical
   scale.

---

## Workstreams

Independent streams that can progress in parallel where dependencies allow.

### WS-1 — Data layer

**Owner:** self · **Status:** complete · **Depends on:** nothing

Schema, roles, RLS, migrations. Complete through Phase 1. Remaining maintenance is
incremental migration authoring as later phases add tables.

### WS-2 — Ingress

**Owner:** self · **Status:** complete · **Depends on:** WS-1

The webhook receiver, verification, durable append, dedupe, credential resolution.
Complete and verified end to end.

### WS-3 — Processing pipeline

**Owner:** self · **Status:** not started · **Depends on:** WS-1, WS-2

Outbox relay, job leasing, routing, status projection, replay tooling.

### WS-4 — Send path

**Owner:** self · **Status:** not started · **Depends on:** WS-3

Send intents, quota, idempotency, Meta client, retries, templates.

### WS-5 — Interface

**Owner:** self · **Status:** not started · **Depends on:** WS-3, WS-4

API and dashboard.

### WS-6 — Operations

**Owner:** self · **Status:** not started · **Depends on:** everything

Observability, backups, secrets, rate limiting, review and go-live.

---

## Task breakdown

### WS-3 — Processing pipeline

| ID | Task | Size | Notes |
| --- | --- | --- | --- |
| 3.1 | Outbox relay loop | M | `FOR UPDATE SKIP LOCKED`, batch size from config |
| 3.2 | Job lease + reclaim | M | Expiry, heartbeat, crash recovery |
| 3.3 | Contact resolution | M | Inbound phone → contact identity → contact, tenant-scoped |
| 3.4 | Conversation upsert | S | One open thread per contact/channel; tested invariant already exists |
| 3.5 | Message persistence | S | Inbound message rows with provider IDs |
| 3.6 | Status projection | M | Monotonic; late `sent` must not regress `read` |
| 3.7 | Orphan status handling | S | Status for an unmapped message: store as orphan, never drop |
| 3.8 | Replay tooling | M | Re-run a delivery or dead-lettered job without manual SQL |

Critical path: 3.1 → 3.2, then 3.3 → 3.4 → 3.5. 3.6–3.8 are parallel.

### WS-4 — Send path

| ID | Task | Size | Notes |
| --- | --- | --- | --- |
| 4.1 | Send-intent state machine | M | States and legal transitions |
| 4.2 | Quota reservation | M | Transactional; unique per intent/meter/recipient |
| 4.3 | Idempotency keys | S | Per tenant and operation |
| 4.4 | Meta client behind an interface | M | Simulator and real API interchangeable |
| 4.5 | Rate limiting | M | Meta's tiered limits; token bucket in Postgres |
| 4.6 | Retry and backoff | M | Exponential, capped, jittered |
| 4.7 | Template CRUD and approval | L | |
| 4.8 | Dry-run mode | S | Full pipeline, no real send |

Critical path: 4.1 → 4.2 → 4.4 → 4.6. 4.7 is independent.

### WS-5 — Interface

| ID | Task | Size | Notes |
| --- | --- | --- | --- |
| 5.1 | Auth and tenancy | M | Every query tenant-scoped |
| 5.2 | Contacts and consent | M | Backed by existing consent evidence tables |
| 5.3 | Inbox | L | |
| 5.4 | Campaign builder | L | |
| 5.5 | Delivery reporting | M | |
| 5.6 | Audit log view | M | Who sent what, under which consent |

---

## Sequencing rules

1. **WS-1 and WS-2 before anything.** They are complete; new work builds on them.
2. **A table gets its RLS in the same migration that creates it.** Never in a
   follow-up. A window where a table exists but is unprotected is a breach.
3. **No send path before the outbox relay.** Sending into an unprocessed queue is
   how messages get lost silently.
4. **Dry-run before real sends.** The pipeline must be provable with no external
   side effects.
5. **Migrations ship with the code that needs them.** Same commit.

---

## Definition of done

A task is done when all of these hold:

- [ ] Behaviour is implemented, not stubbed
- [ ] A test fails without the change and passes with it
- [ ] `tools/verify.sh` passes
- [ ] Failure behaviour is defined and tested, not just the happy path
- [ ] Any new table has RLS, roles and privileges in the same migration
- [ ] Any new query is tenant-scoped, verified by a cross-tenant test
- [ ] New environment variables documented in `README.md`
- [ ] Nothing claims coverage the environment cannot provide

The last two clauses exist because both of this project's biggest current risks
are silent overclaiming, not missing code.

---

## Risks

| Risk | Impact | Likelihood | Mitigation | Status |
| --- | --- | --- | --- | --- |
| Concurrency logic never tested on real Postgres | Silent double-processing or stalled jobs | **High** — PGlite serializes everything | Provision real Postgres before trusting WS-3 | **Open** |
| No Meta account | Cannot send anything | Certain | Simulator behind the same interface; block on credentials | **Open** |
| Premature infrastructure | Rework and ops burden | Low | "Not planned" list in the roadmap, enforced | Mitigated |
| Tenant isolation regression | Data breach | Medium | 29 adversarial RLS checks, run on every change | Mitigated |
| RLS applied late to a new table | Breach window in production | Low | Same-migration rule in the DoD | Mitigated |
| Quota accounting drifts from reality | Over-sending, account bans | Medium | Transactional reservation before send, never after | Open |
| Solo-operator bus factor | Stalled delivery | Certain | Docs in `docs/`, verification scripted | Mitigated |
| Secrets leak into the repo | Credential compromise | Low | Secrets never in code; `.gitignore`; scan before commit | Mitigated |

The concurrency risk is the one that matters most. Everything written so far is
correct and tested; the first genuinely hard part is precisely the part this
environment cannot yet test.

---

## Test strategy by layer

| Layer | Tool | Covers | Current |
| --- | --- | --- | --- |
| Schema/RLS | PGlite in-process (`tools/dbtest/run.mjs`) | Isolation, constraints, privileges, ordering | 29 passing |
| Integration | pgx over PGlite wire protocol | Durable append, dedupe, byte-exactness, roles | 8 passing |
| Unit | Go standard library | HMAC, HTTP contract, envelope parsing | Passing |
| End-to-end | Simulator → service → PGlite → read-back | Full acknowledgement semantics | Passing |
| Concurrency | **Requires real PostgreSQL** | `SKIP LOCKED`, lease reclaim, pool reset | **Not available** |
| Live API | **Requires Meta account** | Real webhook shapes, real send | **Not available** |

The last two rows are the honest boundary of current confidence.

---

## Cadence

No standups, no sprint ceremonies — one operator. Instead:

- **After every task:** `tools/verify.sh`, commit when green.
- **After every phase:** re-read the roadmap, re-check the "not planned" list,
  update this document's risk table.
- **Before any real send:** dry-run evidence first, then one message to a test
  number, then a batch, then production.