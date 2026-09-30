-- 0007_status.sql
-- Provider status observations, independent of the send request.
--
-- v2 section 11: "Meta independently originates receipt callbacks." A status
-- webhook is NOT a reply to your HTTP request, and it can arrive:
--   * out of order
--   * BEFORE the outbound send response has been stored
-- The second case is why send_intent_id is nullable here: the reconciler
-- parks an orphan observation and joins it when the mapping appears
-- (v2 section 12, acceptance case "Independent receipt races HTTP response").

CREATE TABLE status_observations (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id           uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  channel_account_id  uuid NOT NULL REFERENCES channel_accounts(id) ON DELETE CASCADE,
  provider_message_id text NOT NULL,
  -- NULL until the outbound mapping exists. Do not guess it.
  send_intent_id      uuid,
  message_id          uuid,
  conversation_id     uuid,
  status              text NOT NULL
                      CHECK (status IN ('accepted','sent','delivered','read','failed','deleted')),
  -- The dedupe identity is (provider message id, status, provider timestamp).
  -- v2 section 9 is explicit that this is NOT the same thing as the partition
  -- key, which is why the two are separate concerns here.
  provider_timestamp  timestamptz,
  dedupe_key          text NOT NULL,
  observed_at         timestamptz NOT NULL DEFAULT now(),
  -- Kept as data, not as a projection. v2 section 15: derive display state
  -- without discarding late evidence, and never regress a 'read' projection
  -- because an older 'sent' arrives afterwards.
  error_code          text,
  raw_ref             text,
  created_at          timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, send_intent_id)
    REFERENCES send_intents (tenant_id, id) ON DELETE SET NULL,
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, dedupe_key)
);
-- Orphan-first scan: unmatched observations the reconciler must join.
CREATE INDEX status_observations_orphan_idx
  ON status_observations (tenant_id, provider_message_id, observed_at)
  WHERE send_intent_id IS NULL;
CREATE INDEX status_observations_intent_idx
  ON status_observations (tenant_id, send_intent_id, observed_at);
CREATE INDEX status_observations_conversation_idx
  ON status_observations (tenant_id, conversation_id, provider_timestamp DESC);

-- Explicit projection so the inbox does not recompute state per row read.
-- Kept in the same transaction as the observation insert so it cannot drift.
CREATE TABLE message_delivery_projection (
  tenant_id           uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  message_id          uuid NOT NULL,
  send_intent_id      uuid,
  -- Monotonic: a lower-status arrival must never overwrite a higher one.
  rank                int NOT NULL DEFAULT 0,
  latest_status       text NOT NULL,
  latest_provider_timestamp timestamptz,
  updated_at          timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, message_id),
  FOREIGN KEY (tenant_id, message_id) REFERENCES messages (tenant_id, id) ON DELETE CASCADE
);

-- Rank guard: a projection may only move forward.
CREATE OR REPLACE FUNCTION app_delivery_rank(p_status text) RETURNS int
LANGUAGE sql IMMUTABLE AS $$
  SELECT CASE p_status
    WHEN 'accepted' THEN 1
    WHEN 'sent'    THEN 2
    WHEN 'delivered' THEN 3
    WHEN 'read'    THEN 4
    WHEN 'failed'  THEN 5   -- terminal, but must not be undone by a late 'sent'
    WHEN 'deleted' THEN 6
    ELSE 0
  END
$$;

CREATE OR REPLACE FUNCTION app_apply_status_observation() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  v_status text;
  v_rank   int;
  v_ts     timestamptz;
  v_msg    uuid;
BEGIN
  SELECT status, provider_timestamp, message_id
    INTO v_status, v_ts, v_msg
    FROM status_observations
   WHERE id = NEW.id;

  IF v_msg IS NULL THEN
    RETURN NEW;   -- still an orphan; nothing to project yet
  END IF;

  v_rank := app_delivery_rank(v_status);

  INSERT INTO message_delivery_projection
    (tenant_id, message_id, send_intent_id, rank, latest_status, latest_provider_timestamp)
  VALUES (NEW.tenant_id, v_msg, NEW.send_intent_id, v_rank, v_status, v_ts)
  ON CONFLICT (tenant_id, message_id) DO UPDATE
    SET rank = EXCLUDED.rank,
        latest_status = EXCLUDED.latest_status,
        latest_provider_timestamp = EXCLUDED.latest_provider_timestamp,
        send_intent_id = COALESCE(message_delivery_projection.send_intent_id, EXCLUDED.send_intent_id),
        updated_at = now()
    -- v2 section 15: out-of-order arrival is expected. Only apply a strictly
    -- later status; a late 'sent' must not undo a recorded 'read'.
    WHERE EXCLUDED.rank > message_delivery_projection.rank
       OR (EXCLUDED.rank = message_delivery_projection.rank
           AND EXCLUDED.latest_provider_timestamp > message_delivery_projection.latest_provider_timestamp);

  RETURN NEW;
END
$$;

CREATE TRIGGER status_observations_project
  AFTER INSERT ON status_observations
  FOR EACH ROW EXECUTE FUNCTION app_apply_status_observation();
