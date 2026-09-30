# Roadmap

What gets built, in what order, and where each phase actually stands.

No calendar dates. Sequencing is driven by dependencies, and inventing dates for
work that has not started would be fiction. Convert to dates only once a phase has
a real estimate and the blockers below are resolved.

**Legend:** `[x]` done and verified · `[~]` in progress · `[ ]` not started

---

## Phase 0 — Foundations (done)

The schema has to exist before anything can be written against it, and it has to
be right because every migration after this one builds on it.

- [x] Architecture and technology decisions (`docs/`)
- [x] 11 migrations: identity, channels, contacts, conversations, send path, status,
      ingress, outbox, roles, RLS
- [x] Extension-free schema — no `pgcrypto`, no `citext` — so it runs on stock
      PostgreSQL 13+ and under PGlite
- [x] Roles defined before RLS, so policies have something to attach to
- [x] Tenant-prefixed composite foreign keys throughout, so a row cannot reference
      another tenant's channel or contact even by primary key
- [x] RLS with transaction-local `app.tenant_id`; unscoped pooled connections read
      zero rows
- [x] `app_ingress` restricted to inserts plus routing reads
- [x] Schema/RLS suite: 29 checks, all passing

**Why this was first:** retrofitting tenant isolation onto a live schema means
migrating data under unknown constraints. Doing it once, at the start, is cheaper
than doing it twice.

---

## Phase 1 — Inbound webhook slice (done)

The vertical slice that proves the core idea end to end.

- [x] HMAC-SHA256 verification over exact raw bytes, constant-time compare,
      legacy SHA-1 rejected
- [x] HTTP contract: `200` only after commit; `503` on append or credential
      failure; `401`, `404`, `413`, `400`, `405` for the rest
- [x] Durable pgx appender with delivery-key and item-fingerprint dedupe, using
      savepoints so a duplicate does not abort the batch
- [x] Authenticated malformed payloads stored and acknowledged; unauthenticated
      payloads never stored
- [x] Credential resolution from `credential_ref` (currently `env:` references)
- [x] Memory appender, gated behind an explicit opt-in that refuses to start
      otherwise
- [x] Simulator: message, status, error, malformed, empty, fixed retries, tampered
      bodies
- [x] pgx integration tests: 8 passing
- [x] Independent read-back verifier that re-derives stored HMACs from disk
- [x] `tools/verify.sh`

---

## Phase 2 — Durable processing pipeline (next)

Turn stored deliveries into work. Nothing here is user-visible; it is the
machinery the user-facing phases depend on.

- [ ] **Outbox relay.** Claim committed ingress rows with `FOR UPDATE SKIP LOCKED`
      and hand them to workers. Multiple workers, no double-processing.
- [ ] **Job leases.** Leased jobs with expiry and reclaim. A crashed worker's jobs
      become available again instead of stalling forever.
- [ ] **Routing and tenant resolution.** Turn inbound events into contacts,
      conversations and messages with the tenant already resolved.
- [ ] **Status projection.** Apply delivery status without letting a late `sent`
      regress a conversation already marked `read`.
- [ ] **Replay tooling.** Re-run a failed delivery or a dead-lettered job without
      hand-editing the database.

**Blocked on:** a real PostgreSQL server. `SKIP LOCKED` and lease reclaim cannot be
proven under PGlite, which serializes every query. The logic gets written and
reviewed, but it does not get *trusted* until it runs against real concurrency.

---

## Phase 3 — Outbound send path

The reason the platform exists.

- [ ] Send-intent state machine: draft, approved, sending, sent, failed
- [ ] Transactional quota reservation, unique per intent/meter/recipient
- [ ] Idempotency keys per tenant and operation, so a retried request cannot
      double-send
- [ ] Meta Cloud API client, with the transport abstracted behind an interface so
      the simulator and the real API are interchangeable in tests
- [ ] Rate limiting and per-message retry with exponential backoff
- [ ] Template management and approval workflow
- [ ] Dry-run mode: full pipeline, no real send

**Blocked on:** Meta Business verification, a WABA, an app secret and a test
number. Nothing sends until those exist.

---

## Phase 4 — API and dashboard

The operator surface.

- [ ] TypeScript API: auth, tenants, contacts, conversations, campaigns, templates
- [ ] Next.js dashboard: contact lists, conversation inbox, campaign builder,
      delivery status
- [ ] Consent management UI, backed by the consent evidence already in the schema
- [ ] Audit log view: who sent what, to whom, under which consent

---

## Phase 5 — Production hardening

Before real customers.

- [ ] Real PostgreSQL: migrations, `SKIP LOCKED` and lease races verified under
      concurrent workers, connection pool tuning
- [ ] Observability: structured logs, metrics on ingress latency and dedupe rate,
      traces
- [ ] Backup and restore, tested by actually restoring
- [ ] Secret manager integration behind `credential_ref`
- [ ] Rate limiting and abuse protection on the webhook endpoint
- [ ] Meta app review and go-live checklist

---

## Explicitly not planned

Recorded so they do not creep back in:

- Redis / BullMQ — PostgreSQL already does the queueing correctly here
- Kafka / event streaming — premature at this scale
- Temporal — the transactional outbox covers the durability requirement
- Kubernetes — a single service on one box is the target
- Multi-region — no reason until there is a reason
- Microservices beyond services/ and apps/ — two Go services and two TS apps is
  the whole topology

---

## Blockers summary

| Blocker | Blocks | Resolution |
| --- | --- | --- |
| No real PostgreSQL | Phase 2 confidence, Phase 5 | Provision a server; rerun `tools/verify.sh` against it |
| No Meta account | Phase 3 sending | Business verification, WABA, app secret, test number |
| Passphrase-protected SSH key, no agent | pushing | Owner's decision at push time; agent never pushes |