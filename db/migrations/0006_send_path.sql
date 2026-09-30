-- 0006_send_path.sql
-- Send intents, dispatch attempts, quota reservations, recipient windows.
--
-- This is the corrected outbound path. The load-bearing rules:
--   * Intent and outbox row commit BEFORE any provider call (v2 section 11).
--   * A fenced attempt is persisted BEFORE transmission (v2 section 11).
--   * A timeout after possible transmission is 'unknown' and never
--     auto-resends (v2 section 12).
--   * Quota reservations and dispatch ownership are TRANSACTIONAL records.
--     A cache loss must not reset spend or sending allowances (v2 section 20).
--     This is the correction that replaces the Redis-counter approach.

CREATE TABLE send_intents (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  channel_account_id uuid NOT NULL,
  conversation_id   uuid,
  contact_identity_id uuid NOT NULL,
  state             send_intent_state NOT NULL DEFAULT 'queued',
  message_type      text NOT NULL,
  payload_ref       text NOT NULL,
  -- v2 section 16: unique per (tenant, operation, idempotency key). The request
  -- hash is what lets a replay with different content be a 409 rather than a
  -- silent overwrite.
  operation         text NOT NULL DEFAULT 'send_message',
  idempotency_key   text NOT NULL,
  request_hash      text NOT NULL,
  -- v2 section 19: the policy decision is recorded on the intent, with its
  -- version and timestamp, so an audit can reconstruct why a send was allowed.
  policy_version    text,
  policy_decision   text CHECK (policy_decision IN ('allow','block','defer')),
  policy_reason     text,
  policy_decided_at timestamptz,
  -- Generation of conversation ownership at the time the intent was created.
  -- Rechecked immediately before transmission; a bot->agent takeover bumps
  -- ownership_generation and strands the intent as 'blocked'.
  ownership_generation bigint NOT NULL DEFAULT 0,
  -- Human takeover also cancels; this records it explicitly.
  cancelled_reason  text,
  -- Deferred work stays durable with a concrete due_at (v2 section 12).
  due_at            timestamptz,
  expires_at        timestamptz,
  provider_message_id text,
  next_attempt_number int NOT NULL DEFAULT 1,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),

  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, channel_account_id)
    REFERENCES channel_accounts (tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, contact_identity_id)
    REFERENCES contact_identities (tenant_id, id) ON DELETE CASCADE,
  UNIQUE (tenant_id, operation, idempotency_key)
    DEFERRABLE INITIALLY DEFERRED
);
-- The admission scan: eligible deferred work due now, oldest first.
CREATE INDEX send_intents_admission_idx
  ON send_intents (tenant_id, state, due_at)
  WHERE state IN ('queued','deferred');
CREATE INDEX send_intents_conversation_idx
  ON send_intents (tenant_id, conversation_id, created_at DESC);
CREATE INDEX send_intents_provider_idx
  ON send_intents (tenant_id, provider_message_id)
  WHERE provider_message_id IS NOT NULL;
-- Reconciliation sweep: intents stuck in 'unknown' past their review deadline.
CREATE INDEX send_intents_unknown_idx
  ON send_intents (tenant_id, updated_at)
  WHERE state = 'unknown';
CREATE TRIGGER send_intents_touch BEFORE UPDATE ON send_intents
  FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();

-- One row per transmission attempt, kept separate from the intent. v2 section
-- 12: "A failed attempt is not always a failed intent."
CREATE TABLE dispatch_attempts (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  send_intent_id    uuid NOT NULL,
  attempt_number    int  NOT NULL CHECK (attempt_number > 0),
  state             attempt_state NOT NULL DEFAULT 'claimed',
  -- v2 section 11: the fence is recorded BEFORE the network call. If this row
  -- is 'claimed' and old, the request may or may not have gone out -- that is
  -- the definition of an ambiguous window.
  claim_generation  bigint NOT NULL,
  ownership_generation bigint NOT NULL,
  -- Conservative hold on spend/capacity while a call is in flight (v2 s. 12).
  provider_message_id text,
  http_status       int,
  error_code        text,
  error_detail      text,
  -- Opaque correlation value, where the API version supports it. v2 section 11
  -- flags this as version-dependent; nullable until verified.
  correlation_ref   text,
  started_at        timestamptz NOT NULL DEFAULT now(),
  finished_at       timestamptz,
  FOREIGN KEY (tenant_id, send_intent_id)
    REFERENCES send_intents (tenant_id, id) ON DELETE CASCADE,
  UNIQUE (send_intent_id, attempt_number),
  UNIQUE (tenant_id, id)
);
CREATE INDEX dispatch_attempts_intent_idx
  ON dispatch_attempts (tenant_id, send_intent_id, attempt_number DESC);
-- Any attempt left 'claimed' past this age is an unresolved send window.
CREATE INDEX dispatch_attempts_inflight_idx
  ON dispatch_attempts (tenant_id, started_at)
  WHERE state = 'claimed';
CREATE INDEX dispatch_attempts_provider_idx
  ON dispatch_attempts (tenant_id, provider_message_id)
  WHERE provider_message_id IS NOT NULL;

-- ------------------------------------------------------------------ quotas
-- v2 section 20: capacity, quality and restrictions are DIFFERENT meters and
-- must not be collapsed into one boolean. Reservations are transactional and
-- unique, so concurrent cells cannot double-spend one allowance.

CREATE TABLE quota_reservations (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  send_intent_id    uuid NOT NULL,
  scope_kind        provider_scope_kind NOT NULL,
  scope_id          text NOT NULL,
  meter             text NOT NULL
                    CHECK (meter IN (
                      'unique_recipients_24h',   -- rolling daily allowance
                      'requests_per_second',
                      'throughput',
                      'spend')),
  -- Recipient-scoped meters key on the recipient so a second message to the
  -- same person cannot consume the unique-recipient allowance twice.
  recipient_key     text,
  units             int NOT NULL CHECK (units > 0),
  state             reservation_state NOT NULL DEFAULT 'held',
  expires_at        timestamptz,
  settled_at        timestamptz,
  created_at        timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, send_intent_id)
    REFERENCES send_intents (tenant_id, id) ON DELETE CASCADE,
  UNIQUE (tenant_id, id),
  -- "unique logical reservation" (v2 section 20). Re-running the gate cannot
  -- mint a second hold for the same intent/meter/recipient.
  UNIQUE (send_intent_id, meter, recipient_key)
);
CREATE INDEX quota_reservations_scope_idx
  ON quota_reservations (scope_kind, scope_id, meter, state);
CREATE INDEX quota_reservations_held_idx
  ON quota_reservations (tenant_id, expires_at)
  WHERE state = 'held';

-- The replacement for a Redis rolling counter. One row per (scope, window,
-- recipient); inserting consumes the allowance atomically. A Redis flush
-- cannot silently reset it, which is precisely the failure v2 section 20
-- calls out when it rejects the counter approach.
CREATE TABLE recipient_allowances (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  channel_account_id uuid NOT NULL REFERENCES channel_accounts(id) ON DELETE CASCADE,
  -- Identity, not phone number: the provider participant id is the stable key.
  recipient_key     text NOT NULL,
  window_started_at timestamptz NOT NULL,
  window_ends_at    timestamptz NOT NULL,
  observed_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (channel_account_id, recipient_key, window_started_at)
);
CREATE INDEX recipient_allowances_window_idx
  ON recipient_allowances (channel_account_id, window_ends_at);
