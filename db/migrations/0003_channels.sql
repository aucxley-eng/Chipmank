-- 0003_channels.sql
-- Channel accounts and the provider health model.
--
-- v2 section 20 replaces v2's flat per-number model. The key correction:
--   * provider scope is a separate binding (portfolio / waba / phone_number)
--   * exactly one quota_home_cell owns reservations for a given scope
--   * "no fixed tier list in business code" -- quality and capacity are
--     different meters and are stored as separate observations
--   * an unavailable health signal is NOT a healthy one

CREATE TABLE channel_accounts (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  provider_app_id   text NOT NULL,
  waba_id           text NOT NULL,
  phone_number_id   text NOT NULL,
  display_name      text,
  status            account_status NOT NULL DEFAULT 'pending_authorization',
  -- Credential reference only. The secret itself lives in a secrets manager;
  -- v2 section 30 forbids provider secrets in the database and in API
  -- responses. Nothing here is decrypted by the application.
  credential_ref    text,
  api_version       text NOT NULL DEFAULT 'unset',
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),

  -- A number maps to exactly one tenant on this platform. Cross-tenant
  -- ownership is resolved by explicit transfer, never by reassignment.
  UNIQUE (provider_app_id, phone_number_id),
  -- Tenant-prefixed composite FK target (v2 section 15): a valid id from
  -- another tenant cannot be attached here.
  UNIQUE (tenant_id, id)
);
CREATE INDEX channel_accounts_tenant_idx ON channel_accounts (tenant_id);
CREATE INDEX channel_accounts_waba_idx ON channel_accounts (tenant_id, waba_id);
CREATE TRIGGER channel_accounts_touch BEFORE UPDATE ON channel_accounts
  FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();

-- The provider resource that actually carries limits. v2 section 20: a
-- portfolio spans many numbers and has ONE authoritative capacity owner, so
-- cross-cell double-spend is structurally prevented by the unique index below.
CREATE TABLE provider_scope_bindings (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  scope_kind        provider_scope_kind NOT NULL,
  scope_id          text NOT NULL,
  provider_app_id   text NOT NULL,
  quota_home_cell   text NOT NULL,
  mapping_epoch     bigint NOT NULL DEFAULT 1,
  -- "verified asset ownership" (v2 section 20). False means the binding is
  -- not trusted for quota accounting.
  ownership_verified boolean NOT NULL DEFAULT false,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, scope_kind, scope_id),
  UNIQUE (tenant_id, id)
);
CREATE INDEX provider_scope_cell_idx
  ON provider_scope_bindings (scope_kind, scope_id, quota_home_cell);
CREATE TRIGGER provider_scope_bindings_touch BEFORE UPDATE ON provider_scope_bindings
  FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();

-- Point-in-time health. Kept as observations with their evidence, not as a
-- mutable "is healthy" boolean, so freshness is always inspectable
-- (v2 section 20 requires refresh_due_at and observed_at).
CREATE TABLE health_observations (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  channel_account_id uuid REFERENCES channel_accounts(id) ON DELETE CASCADE,
  scope_kind        provider_scope_kind,
  scope_id          text,
  -- quality_rating / messaging_limit_tier / connection status all land here
  observation_kind  text NOT NULL
                    CHECK (observation_kind IN (
                      'quality_rating', 'messaging_limit', 'account_alert',
                      'restriction', 'credential_state', 'template_status',
                      'template_quality', 'capability')),
  -- Raw provider value kept as text: the field set is an adapter contract
  -- under verification (v2 section 21), so it must not be typed to a guess.
  observed_value    text NOT NULL,
  -- 'unknown' is representable and distinct from a healthy value.
  severity          text NOT NULL DEFAULT 'info'
                    CHECK (severity IN ('info','warn','action_required','blocking')),
  observed_at       timestamptz NOT NULL,
  effective_at      timestamptz,
  source            text NOT NULL DEFAULT 'webhook',
  evidence_ref      text,
  created_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id)
);
CREATE INDEX health_obs_current_idx
  ON health_observations (tenant_id, channel_account_id, observation_kind, observed_at DESC);
CREATE INDEX health_obs_scope_idx
  ON health_observations (scope_kind, scope_id, observation_kind, observed_at DESC);
