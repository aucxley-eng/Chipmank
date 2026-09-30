-- 0001_foundation.sql
-- Extensions, shared domains, and the tenant-scoping contract.
--
-- Tenant scoping contract (Architecture v2 section 16, 30):
--   Every tenant-owned relation carries tenant_id NOT NULL and is protected by
--   row-level security. The application sets app.tenant_id inside the
--   transaction and resets it before returning a pooled connection.

-- Case-insensitive uniqueness is expressed with lower() unique indexes rather
-- than the citext extension: same behaviour, one fewer extension to enable,
-- and portable across every Postgres provider (including in-process PGlite,
-- which the schema test harness uses).

-- ---------------------------------------------------------------- domains

-- Provider resource scope. v2 section 20 makes portfolio scope first-class:
-- one authoritative capacity owner may span many numbers.
CREATE DOMAIN provider_scope_kind AS text
  CHECK (VALUE IN ('portfolio', 'waba', 'phone_number'));

-- Account lifecycle. Mirrors v2 section 8 and section 5 (v2 adds health tiers).
CREATE DOMAIN account_status AS text
  CHECK (VALUE IN (
    'pending_authorization', 'pending_verification', 'configuring',
    'testing', 'active', 'degraded', 'restricted', 'revoked', 'disconnected'
  ));

-- Send intent state machine. v2 section 12 is authoritative: UNKNOWN is
-- terminal-until-evidence and never returns to automatic sending.
CREATE DOMAIN send_intent_state AS text
  CHECK (VALUE IN (
    'queued',      -- durable, awaiting admission
    'deferred',    -- policy said wait; durable, has due_at
    'blocked',     -- policy refused; terminal, with reason
    'expired',     -- past expires_at before dispatch; terminal
    'dispatching', -- fenced attempt claimed, in flight
    'accepted',    -- provider returned 2xx + provider_message_id
    'failed',      -- known permanent rejection; terminal
    'unknown',     -- ambiguous outcome; reconcile only, never auto-resend
    'cancelled'    -- explicit cancellation; terminal
  ));

-- One attempt row per (intent, attempt_number). 'unknown' mirrors the intent:
-- the request may or may not have reached the provider.
CREATE DOMAIN attempt_state AS text
  CHECK (VALUE IN (
    'claimed', 'accepted', 'rejected_permanent',
    'rejected_retryable', 'unknown', 'abandoned'
  ));

-- v2 section 20: "Unknown/unavailable is distinct from healthy."
CREATE DOMAIN reachability AS text
  CHECK (VALUE IN ('unknown', 'observed_reachable', 'observed_unreachable'));

CREATE DOMAIN reservation_state AS text
  CHECK (VALUE IN ('held', 'settled', 'released', 'expired'));

-- ---------------------------------------------------------------- helpers

-- Resolve the caller's tenant. current_setting(..., true) returns NULL when
-- unset, so a pooled connection that was never scoped sees no rows rather
-- than all rows. That fail-closed default is the point of RLS here.
CREATE OR REPLACE FUNCTION app_current_tenant() RETURNS uuid
LANGUAGE sql STABLE AS $$
  SELECT NULLIF(current_setting('app.tenant_id', true), '')::uuid
$$;

CREATE OR REPLACE FUNCTION app_touch_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END
$$;
