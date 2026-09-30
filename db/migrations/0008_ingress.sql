-- 0008_ingress.sql
-- Durable ingress acceptance.
--
-- v2 section 5 ("Regional ingress acknowledgment contract"):
--   "A forwarding ingress is not a queue. It acknowledges only after every
--    accepted item has a durable receipt in its authorized destination region."
--
-- This is the table that makes a 200 honest. There is no in-memory hand-off.
--
-- NOTE ON RLS: this table is intentionally NOT tenant-filtered, because at
-- ingress time the platform knows only phone_number_id and waba_id -- the
-- conversation and the tenant are resolved downstream. It is protected by
-- ROLE instead, and the ingress role is the only runtime role granted on it.
-- Tenant resolution happens in tenant_routing_bindings below, after which
-- every downstream table is tenant-scoped.

CREATE TABLE ingress_items (
  id                bigserial PRIMARY KEY,
  provider_app_id   text NOT NULL,
  -- Fingerprint of ONE webhook item, derived from provider identifiers and
  -- event-specific stable fields. v2 section 5 is explicit: identity must not
  -- come from the retry arrival time, or every redelivery looks like a new
  -- item and dedupe stops working.
  item_fingerprint  text NOT NULL,
  phone_number_id   text,
  waba_id           text,
  -- Raw bytes are preserved because signature verification is over the
  -- ORIGINAL body: re-serializing JSON changes the bytes and breaks the HMAC.
  -- v2 section 9 ("Preserve raw bytes until verification").
  payload           bytea NOT NULL,
  signature_header  text,
  app_secret_ref    text NOT NULL,
  signature_verified boolean NOT NULL DEFAULT false,
  -- tenant_id is null until routing resolves the number to a tenant. A null
  -- here is not a failure; it is simply not yet resolved.
  tenant_id         uuid REFERENCES tenants(id) ON DELETE SET NULL,
  channel_account_id uuid,
  placement_epoch   bigint,
  -- v2 section 5: committed log position, the thing a receipt points at.
  committed_position bigint NOT NULL DEFAULT 0,
  received_at       timestamptz NOT NULL DEFAULT now(),
  normalized_at     timestamptz,
  -- Set when the item is recognised but cannot be routed.
  quarantine_reason text,
  UNIQUE (provider_app_id, item_fingerprint)
);
-- The normalizer's scan: verified, unresolved, unprocessed.
CREATE INDEX ingress_items_pending_idx
  ON ingress_items (received_at)
  WHERE normalized_at IS NULL AND signature_verified;

-- committed_position is the log position of this item, and it is the serial id.
-- It is filled by a trigger rather than by the application so that the value is
-- correct no matter which writer inserts the row, and so the ingest role does
-- not need UPDATE on this table just to stamp a position. Column defaults are
-- applied before BEFORE INSERT triggers run, so NEW.id is already assigned.
CREATE OR REPLACE FUNCTION app_assign_committed_position()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  NEW.committed_position := NEW.id;
  RETURN NEW;
END
$$;

CREATE TRIGGER ingress_items_assign_position
  BEFORE INSERT ON ingress_items
  FOR EACH ROW EXECUTE FUNCTION app_assign_committed_position();
-- Unresolved but verified: the operator-attention queue.
CREATE INDEX ingress_items_quarantine_idx
  ON ingress_items (received_at)
  WHERE quarantine_reason IS NOT NULL;

-- The routing table the ingress path consults. Maps provider identifiers to a
-- tenant BEFORE the conversation exists, which is what lets acknowledgement
-- stay tenant-bound even though the conversation does not exist yet.
CREATE TABLE tenant_routing_bindings (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  provider_app_id   text NOT NULL,
  phone_number_id   text NOT NULL,
  tenant_id         uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  channel_account_id uuid NOT NULL,
  -- Bumped on placement change. An item stamped with a stale epoch is
  -- rejected by the old home region rather than written to a second writer
  -- (v2 section 4, section 5).
  placement_epoch   bigint NOT NULL DEFAULT 1,
  updated_at        timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, channel_account_id)
    REFERENCES channel_accounts (tenant_id, id) ON DELETE CASCADE,
  UNIQUE (provider_app_id, phone_number_id)
);
CREATE INDEX tenant_routing_epoch_idx
  ON tenant_routing_bindings (phone_number_id, placement_epoch DESC);

-- Cross-region durable acceptance. v2 section 5: no unapproved cross-region
-- BODY spool -- this stores the receipt and the destination, not the message
-- body. A forwarding region holds a signed envelope reference, nothing more.
CREATE TABLE ingress_receipts (
  id                bigserial PRIMARY KEY,
  receipt_ref       uuid NOT NULL DEFAULT gen_random_uuid(),
  provider_app_id   text NOT NULL,
  item_fingerprint  text NOT NULL,
  destination_region text NOT NULL,
  destination_tenant uuid REFERENCES tenants(id) ON DELETE SET NULL,
  placement_epoch   bigint NOT NULL,
  committed_position bigint NOT NULL,
  created_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider_app_id, item_fingerprint, destination_region)
);
CREATE INDEX ingress_receipts_pending_idx ON ingress_receipts (created_at);

-- Security evidence for rejected requests. v2 section 39: "Invalid signature
-- or forged tenant context -> no domain state change; event rejected and safe
-- security evidence recorded." No payload body, only the digest.
CREATE TABLE ingress_rejections (
  id                bigserial PRIMARY KEY,
  provider_app_id   text,
  source_hint       text,
  reason            text NOT NULL
                    CHECK (reason IN ('bad_signature','oversized_body','malformed_json','unknown_app','rate_limited')),
  payload_sha256    text,
  remote_addr       text,
  observed_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ingress_rejections_recent_idx
  ON ingress_rejections (reason, observed_at DESC);
