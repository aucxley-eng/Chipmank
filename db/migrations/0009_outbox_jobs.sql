-- 0009_outbox_jobs.sql
-- The transactional outbox and the Postgres-backed job queue.
--
-- This replaces Kafka + Temporal for the first slice. The invariants are
-- identical and are stated in v2 sections 9 and 14:
--   * Business row + outbox row commit in ONE transaction. Never publish from
--     inside a business transaction (v2 section 1).
--   * The publisher can crash after publishing and before marking completion,
--     so consumers need a durable processed-event record and idempotent
--     effects. A broker ack alone does not make the DB transaction atomic.
--   * Replays preserve event and action identities.
-- Nothing below weakens any of that; only the transport changes.

CREATE TABLE outbox (
  id              bigserial PRIMARY KEY,
  event_id        uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id       uuid REFERENCES tenants(id) ON DELETE CASCADE,
  aggregate_type  text NOT NULL,
  aggregate_id    uuid NOT NULL,
  -- v2 section 9: relay each aggregate's committed outbox rows in sequence,
  -- and let consumers detect gaps.
  aggregate_seq   bigint,
  event_type      text NOT NULL,
  schema_version  int  NOT NULL DEFAULT 1,
  payload         jsonb NOT NULL,
  occurred_at     timestamptz NOT NULL DEFAULT now(),
  published_at    timestamptz,
  publish_attempts int NOT NULL DEFAULT 0,
  last_error      text
);
-- The relay's hot path: unpublished rows, oldest first.
CREATE INDEX outbox_unpublished_idx ON outbox (id) WHERE published_at IS NULL;
-- Per-aggregate ordering for the relay.
CREATE INDEX outbox_aggregate_idx
  ON outbox (tenant_id, aggregate_type, aggregate_id, aggregate_seq, id);
CREATE UNIQUE INDEX outbox_event_id_idx ON outbox (event_id);

-- Consumer-side dedupe. v2 section 9: "commit a consumer's processed-event
-- marker with its business write." The marker lives here; the effect commits
-- with the caller's own transaction.
CREATE TABLE processed_events (
  consumer_name  text NOT NULL,
  event_id       uuid NOT NULL,
  processed_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (consumer_name, event_id)
);
CREATE INDEX processed_events_age_idx ON processed_events (processed_at);

-- Durable job queue. Replaces the workflow engine for waits, retries and
-- scheduled work. v2 section 14: state must survive restarts, and a stored
-- wait resumes through a NEW event rather than a live in-memory timer.
CREATE TABLE jobs (
  id              bigserial PRIMARY KEY,
  tenant_id       uuid REFERENCES tenants(id) ON DELETE CASCADE,
  job_type        text NOT NULL,
  -- Stable action identity: retries reuse it so a redelivered job does not
  -- produce a second side effect. Format: tenant/entity/node/iteration.
  action_key      text,
  payload         jsonb NOT NULL DEFAULT '{}'::jsonb,
  state           text NOT NULL DEFAULT 'pending'
                  CHECK (state IN ('pending','claimed','done','failed','cancelled')),
  run_at          timestamptz NOT NULL DEFAULT now(),
  attempts        int  NOT NULL DEFAULT 0,
  max_attempts    int  NOT NULL DEFAULT 5,
  -- Fencing token. A worker whose lease expired fails closed rather than
  -- applying a stale result (v2 section 24: "old dispatchers fail closed
  -- when their short lease expires").
  claim_generation bigint NOT NULL DEFAULT 0,
  claimed_by      text,
  claimed_at      timestamptz,
  lease_expires_at timestamptz,
  last_error      text,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (job_type, action_key)
);
-- The claim scan. Partial index keeps it to pending work only.
CREATE INDEX jobs_claimable_idx
  ON jobs (run_at, id)
  WHERE state = 'pending';
-- Expired-lease recovery: 'claimed' rows whose lease ran out.
CREATE INDEX jobs_expired_lease_idx
  ON jobs (lease_expires_at)
  WHERE state = 'claimed';
CREATE TRIGGER jobs_touch BEFORE UPDATE ON jobs
  FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();

-- Outbox relay and job workers both need "claim a row atomically, do work,
-- mark done, with a fence that survives a crash mid-flight." One function,
-- used by both.
CREATE OR REPLACE FUNCTION app_claim_jobs(
  p_worker    text,
  p_limit     int  DEFAULT 10,
  p_lease_sec int  DEFAULT 60
) RETURNS SETOF jobs
LANGUAGE plpgsql AS $$
DECLARE
  r jobs;
BEGIN
  FOR r IN
    UPDATE jobs j
       SET state           = 'claimed',
           claimed_by       = p_worker,
           claimed_at       = now(),
           lease_expires_at = now() + make_interval(secs => p_lease_sec),
           claim_generation = j.claim_generation + 1,
           attempts         = j.attempts + 1,
           updated_at       = now()
     WHERE j.id IN (
             SELECT id FROM jobs
              WHERE state = 'pending' AND run_at <= now()
              ORDER BY run_at, id
              LIMIT p_limit
              FOR UPDATE SKIP LOCKED   -- concurrent workers never collide
           )
    RETURNING *
  LOOP
    RETURN NEXT r;
  END LOOP;
END
$$;

-- Recover jobs whose worker died mid-flight. Deliberately does NOT retry
-- forever: a job past max_attempts is quarantined for inspection, matching the
-- "quarantine malformed or exhausted items with reason" rule.
CREATE OR REPLACE FUNCTION app_reclaim_expired_jobs() RETURNS int
LANGUAGE plpgsql AS $$
DECLARE n int;
BEGIN
  WITH reclaimed AS (
    UPDATE jobs
       SET state = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
           claimed_by = NULL,
           claimed_at = NULL,
           lease_expires_at = NULL,
           last_error = COALESCE(last_error, 'lease expired; worker presumed dead'),
           updated_at = now()
     WHERE state = 'claimed'
       AND lease_expires_at IS NOT NULL
       AND lease_expires_at < now()
    RETURNING 1
  )
  SELECT count(*) INTO n FROM reclaimed;
  RETURN n;
END
$$;
