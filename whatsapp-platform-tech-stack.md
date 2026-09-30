# WhatsApp Marketing Platform: Recommended Tech Stack

Two versions of the stack: the **enterprise stack** from the v2 architecture PDF, and a **lean start** that a small team can ship with the same core guarantees.

## 1. Stack Comparison

| Layer | Enterprise stack (v2 PDF) | Lean start (recommended) |
|---|---|---|
| Web console | React + TypeScript, React Flow for the bot builder | Same |
| Backend APIs | Typed framework (NestJS or similar) | **TypeScript (NestJS)** for everything at first |
| Hot path (webhooks, sender) | Go | TypeScript workers until load proves Go is needed |
| Database | PostgreSQL per cell, with row-level security | **One managed PostgreSQL**, `tenant_id` on every table, row-level security enabled |
| Cache, rate limits, presence | Redis | Redis |
| Event log | Kafka (MSK) | **Postgres outbox + a job queue** (BullMQ on Redis, or Postgres-based) |
| Workflow engine | Temporal | Flow interpreter in workers, state in Postgres, scheduled jobs for timers |
| Realtime inbox | WebSocket gateways | WebSocket (Socket.IO or native) with Redis pub/sub |
| Object storage | S3 | S3 or any S3-compatible store |
| Search / analytics | Search index + columnar store | Postgres full-text search and aggregates first, ClickHouse later |
| AI | Inference gateway + vector index | Hosted LLM API + `pgvector`, draft-only at first |
| Infrastructure | AWS, EKS, Terraform, several cells across 3 availability zones | One region, managed containers (ECS/Fargate or Cloud Run) or small Kubernetes, Terraform from day one |
| Edge | CloudFront + WAF | Cloudflare (WAF, CDN, DNS) |
| Observability | OpenTelemetry, Prometheus, Grafana | OpenTelemetry + Grafana Cloud or Datadog |
| Authentication | OIDC / SAML / SCIM | Managed identity (Auth0, WorkOS or Clerk) |
| Push notifications | APNs / FCM | Firebase Cloud Messaging (covers iOS and Android) |
| Billing | Usage ledger | Stripe for subscriptions and a usage-based wallet |
| CI/CD | Signed images, canary releases by cell | GitHub Actions, one staging environment, rolling deploys |

## 2. Rules to Keep From Day One

Whichever column you follow, keep these guarantees. They do not depend on the technology:

- Verify Meta's webhook signature on the raw request body.
- Save the event durably **before** acknowledging it to Meta.
- Use idempotency keys on every inbound event and every send.
- Send only through **one outbound gateway** that checks consent, template and window rules.
- Mark a timed-out send as **unknown** and reconcile it. Never blindly resend.
- Store consent evidence (source, time, purpose) for every contact.
- Rate-limit per phone number and track the number's messaging limit and quality rating.
- Encrypt access tokens and enforce tenant isolation in every query.

## 3. Language Choice

Use **TypeScript across the whole stack** for a small team: one language, shared types between frontend and backend, and a large hiring pool. Add **Go** later only if the webhook receiver or sender becomes a measured bottleneck.

## 4. When to Upgrade

Postgres is the single source of truth, so each heavier component can be added later without redesigning the data.

| Add this | When this happens |
|---|---|
| Kafka | Queue throughput or replay needs outgrow Postgres and the job queue |
| Temporal | Flows become long-running and highly branching |
| ClickHouse | Dashboard queries start slowing the main database |
| Multiple cells or regions | A customer needs data isolation, residency, or contractual disaster recovery |
| Go services | The webhook receiver or sender is a measured CPU or latency bottleneck |
| Dedicated search engine | Full-text inbox search outgrows Postgres |

## 5. Open Decisions

- Which cloud provider (AWS, GCP, or another).
- Team size and on-call capacity, which decides between managed containers and Kubernetes.
- Whether you bill Meta charges directly to the customer or through your own wallet.
- Target launch markets, which affect payments, data residency and Meta pricing.

*Verify Meta API versions, webhook events and pricing against the live developer documentation before implementing.*
