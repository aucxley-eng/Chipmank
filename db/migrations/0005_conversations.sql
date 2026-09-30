-- 0005_conversations.sql
-- Conversations and messages.
--
-- v2 section 10 is the ordering contract:
--   Raw ingress carries NO conversation id. The normalizer resolves tenant,
--   contact and conversation inside one transaction, allocates aggregate_seq,
--   and writes the outbox row. Order comes from aggregate_seq, never from
--   arrival order. That is why (tenant_id, conversation_id, aggregate_seq) is
--   the primary read path and the only supported ordering.

CREATE TABLE conversations (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  channel_account_id uuid NOT NULL,
  contact_identity_id uuid NOT NULL,
  -- Tenant-prefixed composite FKs (v2 section 15). Without the tenant column
  -- in the key, a globally-unique id belonging to another tenant would be a
  -- valid reference and isolation would leak through referential integrity.
  FOREIGN KEY (tenant_id, channel_account_id)
    REFERENCES channel_accounts (tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, contact_identity_id)
    REFERENCES contact_identities (tenant_id, id) ON DELETE CASCADE,
  status            text NOT NULL DEFAULT 'open'
                    CHECK (status IN ('open','pending','resolved','closed')),
  -- v2 section 10: use the MAXIMUM valid provider timestamp, never the
  -- last-arriving one, or an out-of-order batch moves the window backwards.
  last_customer_message_at timestamptz,
  -- v2 section 15: the provider's 24h service window is tracked separately
  -- from the ticket lifecycle. A closed ticket is not a closed window.
  service_window_expires_at timestamptz,
  -- v2 section 16: the index inbound high-volume reads use.
  aggregate_seq     bigint NOT NULL DEFAULT 0,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id)
);

-- At most one open thread per (tenant, channel account, contact identity).
-- This is the constraint that lets the normalizer resolve-or-create inside a
-- transaction without a race. DEFERRABLE so the create-then-link pattern can
-- commit atomically.
CREATE UNIQUE INDEX conversations_one_open_per_identity
  ON conversations (tenant_id, channel_account_id, contact_identity_id)
  WHERE status = 'open';
CREATE INDEX conversations_inbox_idx
  ON conversations (tenant_id, status, last_customer_message_at DESC, id);
CREATE INDEX conversations_identity_idx
  ON conversations (tenant_id, contact_identity_id, created_at DESC);
CREATE TRIGGER conversations_touch BEFORE UPDATE ON conversations
  FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();

CREATE TABLE messages (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  conversation_id   uuid NOT NULL,
  channel_account_id uuid NOT NULL,
  -- 'outbound' rows are the platform's own record; the provider's view is
  -- send_intents + dispatch_attempts + status_observations. v2 section 15:
  -- send intents are NOT the same object as observed provider messages.
  direction         text NOT NULL CHECK (direction IN ('inbound','outbound')),
  message_type      text NOT NULL
                    CHECK (message_type IN (
                      'text','image','audio','video','document','sticker',
                      'template','interactive_reply','interactive_list',
                      'cta_url','location','contacts','reaction',
                      'reply_context','catalog','flow_response',
                      'ad_referral','unsupported')),
  -- Bodies live in object storage; the row carries the reference. v2 section
  -- 30 forbids plaintext conversation content in shared caches and logs.
  body_ref          text,
  -- null for locally-originated rows that have not been accepted yet.
  provider_message_id text,
  aggregate_seq     bigint NOT NULL,
  reply_to_message_id uuid,
  -- v2 section 10: occurred_at is provider time, received_at is platform time.
  -- Both exist because provider arrival order is not business event time.
  occurred_at       timestamptz,
  received_at       timestamptz NOT NULL DEFAULT now(),
  metadata          jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at        timestamptz NOT NULL DEFAULT now(),

  FOREIGN KEY (tenant_id, conversation_id)
    REFERENCES conversations (tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, channel_account_id)
    REFERENCES channel_accounts (tenant_id, id) ON DELETE CASCADE,
  UNIQUE (tenant_id, id),
  -- v2 section 16: the inbound dedupe constraint. A redelivered webhook
  -- cannot produce a second canonical message.
  UNIQUE (tenant_id, channel_account_id, provider_message_id)
    DEFERRABLE INITIALLY DEFERRED
);
CREATE INDEX messages_conversation_order_idx
  ON messages (tenant_id, conversation_id, aggregate_seq);
CREATE INDEX messages_recent_idx
  ON messages (tenant_id, created_at DESC);
CREATE INDEX messages_provider_idx
  ON messages (tenant_id, provider_message_id)
  WHERE provider_message_id IS NOT NULL;

-- A reaction is a message relation, not a new bubble (v2 section 13).
CREATE TABLE message_relations (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  source_message_id uuid NOT NULL,
  relation_type     text NOT NULL
                    CHECK (relation_type IN ('reply_to','reaction','forward')),
  target_provider_id text,
  created_at        timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, source_message_id)
    REFERENCES messages (tenant_id, id) ON DELETE CASCADE,
  UNIQUE (tenant_id, id)
);
CREATE INDEX message_relations_target_idx
  ON message_relations (tenant_id, target_provider_id);
