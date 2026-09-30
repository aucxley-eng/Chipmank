-- 0004_contacts_consent.sql
-- Contacts, channel identities, consent evidence.
--
-- v2 section 15: a contact is a person; a contact_identity is that person on
-- one channel. The split is what lets a person exist across channels without
-- conflating them.
--
-- v2 section 17 is the correction that matters here: importing a contact
-- creates an identity, NOT marketing permission, and NOT proof that the number
-- is on WhatsApp.

CREATE TABLE contacts (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  display_name      text,
  lifecycle_stage   text,
  -- Consent is derived from consent_events, never from a boolean here. A
  -- flag on the contact row is exactly the thing that rots.
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id)
);
CREATE INDEX contacts_tenant_idx ON contacts (tenant_id);
CREATE TRIGGER contacts_touch BEFORE UPDATE ON contacts
  FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();

CREATE TABLE contact_identities (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  contact_id        uuid NOT NULL,
  channel_account_id uuid NOT NULL,
  -- Composite, tenant-prefixed FKs. A plain FK to contact_identities(id)
  -- would accept another tenant's identity id, because the id is globally
  -- unique and therefore "valid". v2 section 15 requires the tenant prefix so
  -- that a valid id from the wrong tenant is still rejected.
  FOREIGN KEY (tenant_id, contact_id)
    REFERENCES contacts (tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, channel_account_id)
    REFERENCES channel_accounts (tenant_id, id) ON DELETE CASCADE,
  -- v2 section 16: provider participant identity is preserved independently of
  -- the phone number, which may be absent or change.
  provider_participant_id text,
  phone_e164        text,
  -- Original input preserved verbatim so a normalization bug is diagnosable
  -- and reversible (v2 section 17).
  phone_raw         text,
  -- v2 section 17: formatting validation is not a WhatsApp registration
  -- lookup. Absent evidence stays 'unknown' and stays that way.
  reachability      reachability NOT NULL DEFAULT 'unknown',
  reachability_evidence_type text,
  reachability_error_code    text,
  reachability_observed_at   timestamptz,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, channel_account_id, phone_e164)
    DEFERRABLE INITIALLY DEFERRED
);
-- Provider participant lookup happens on every inbound, so it gets its own index.
CREATE INDEX contact_identities_participant_idx
  ON contact_identities (tenant_id, channel_account_id, provider_participant_id);
CREATE INDEX contact_identities_contact_idx
  ON contact_identities (tenant_id, contact_id);
CREATE TRIGGER contact_identities_touch BEFORE UPDATE ON contact_identities
  FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();

-- Append-only evidence. v2 section 19: identity, purpose, channel, source,
-- time, policy version, revocation. Absence of a marketing row here is what
-- makes a contact ineligible -- not presence of anything elsewhere.
CREATE TABLE consent_events (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id           uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  contact_identity_id uuid NOT NULL REFERENCES contact_identities(id) ON DELETE CASCADE,
  purpose             text NOT NULL
                      CHECK (purpose IN ('marketing','service','transactional')),
  channel             text NOT NULL,
  source              text NOT NULL,
  policy_text_version text NOT NULL,
  observed_at         timestamptz NOT NULL,
  revoked_at          timestamptz,
  evidence            jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at          timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id)
);
-- The authorization read: latest non-revoked consent for one purpose.
CREATE INDEX consent_lookup_idx
  ON consent_events (tenant_id, contact_identity_id, purpose, observed_at DESC);
