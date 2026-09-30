-- 0011_rls.sql
-- Row-level security: the enforcement layer for tenant isolation.
--
-- v2 section 30: "RLS is defense in depth, not a substitute for application
-- authorization" -- but it must be a real layer, not a comment. Every
-- tenant-owned table gets a policy, and every table gets FORCE so that even
-- the table owner is subject to it.
--
-- The fail-closed property: app_current_tenant() returns NULL when
-- app.tenant_id is unset, and a comparison to NULL is never true. A pooled
-- connection that was not scoped for the current transaction therefore sees
-- ZERO rows rather than ALL rows.

DO $$
DECLARE
  t text;
  tenant_tables text[] := ARRAY[
    'teams',
    'tenant_memberships',
    'channel_accounts',
    'provider_scope_bindings',
    'health_observations',
    'contacts',
    'contact_identities',
    'consent_events',
    'conversations',
    'messages',
    'message_relations',
    'send_intents',
    'dispatch_attempts',
    'quota_reservations',
    'recipient_allowances',
    'status_observations',
    'message_delivery_projection',
    'outbox',
    'jobs'
  ];
BEGIN
  FOREACH t IN ARRAY tenant_tables LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format($f$
      CREATE POLICY %1$I_tenant_isolation ON %1$I
        USING      (tenant_id = app_current_tenant())
        WITH CHECK (tenant_id = app_current_tenant())
    $f$, t);
  END LOOP;
END
$$;

-- tenants is keyed on its own id rather than a tenant_id column.
ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenants FORCE ROW LEVEL SECURITY;
CREATE POLICY tenants_tenant_isolation ON tenants
  USING      (id = app_current_tenant())
  WITH CHECK (id = app_current_tenant());

-- ---------------------------------------------------------------- cross-tenant
-- These are deliberately excluded from tenant RLS and are the most likely
-- place for a real isolation bug, so they get explicit grants instead
-- (see 0011_roles.sql). Normal runtime roles have NO access at all.
--
--   ingress_items          -- tenant unknown until routing resolves it
--   ingress_receipts       -- cross-region receipts
--   ingress_rejections     -- security evidence
--   tenant_routing_bindings-- the resolution table itself
--   users                  -- global identity, memberships are tenant-scoped
--
-- Access is expressed as narrowly-scoped grants to app_ingress, and there is
-- deliberately NO "deny everything" policy. With RLS enabled, a role matching
-- no policy is already denied, so naming only the access that exists means a
-- role created later is denied by default instead of inheriting a permissive
-- policy written for someone else.
--
-- Note that RLS is ENABLEd but not FORCEd on these four tables. FORCE is used
-- on tenant-owned tables above, but here the normalizer reaches ingress_items
-- through app_next_ingress_batch, a SECURITY DEFINER function owned by the
-- migration role. FORCE would subject that owner to RLS and break the function.
-- The isolation guarantee there does not rest on RLS at all: the function's own
-- WHERE clause filters on app_current_tenant(), which reads a transaction-local
-- setting the caller cannot spoof.

-- app_ingress appends raw deliveries. It gets INSERT and deliberately not
-- SELECT: reading raw inbound bodies back is how message content would leak
-- into a service that has no need to see it.
--
-- The absence of SELECT has a non-obvious consequence worth recording, because
-- the obvious way to write the dedupe insert does not work here. Writing
--
--   INSERT ... ON CONFLICT (provider_app_id, item_fingerprint) DO NOTHING
--
-- requires BOTH SELECT privilege on the table (true even for DO NOTHING) and an
-- applicable SELECT row-level policy. Granting the privilege only moves the
-- failure from "permission denied" to "violates row-level security policy",
-- and granting a SELECT policy would expose every raw inbound body to the
-- ingest role. So the append path inserts plainly and treats SQLSTATE 23505
-- from the unique index as the duplicate signal, rolling back to a savepoint so
-- the surrounding transaction survives. Dedupe is index-enforced either way, so
-- concurrent redeliveries still cannot both insert.
ALTER TABLE ingress_items ENABLE ROW LEVEL SECURITY;
CREATE POLICY ingress_items_ingest ON ingress_items
  FOR INSERT TO app_ingress
  WITH CHECK (true);

-- Rejections are security evidence: written once, never updated, never read
-- back through a tenant-scoped path.
ALTER TABLE ingress_rejections ENABLE ROW LEVEL SECURITY;
CREATE POLICY ingress_rejections_ingest ON ingress_rejections
  FOR INSERT TO app_ingress
  WITH CHECK (true);

ALTER TABLE ingress_receipts ENABLE ROW LEVEL SECURITY;
CREATE POLICY ingress_receipts_ingest ON ingress_receipts
  FOR INSERT TO app_ingress
  WITH CHECK (true);

-- Routing is the one cross-tenant table app_ingress must READ. It maps a
-- provider phone number to a tenant before any conversation exists, which is
-- what lets acknowledgement stay tenant-bound while the conversation is still
-- unresolved. INSERT stays denied: only an operator may rebind a number.
ALTER TABLE tenant_routing_bindings ENABLE ROW LEVEL SECURITY;
CREATE POLICY routing_bindings_resolve ON tenant_routing_bindings
  FOR SELECT TO app_ingress
  USING (true);

-- users: a signed-in user must be able to resolve their own record without
-- being granted a tenant. Scoped to the authenticated subject only.
ALTER TABLE users ENABLE ROW LEVEL SECURITY;
CREATE POLICY users_self ON users
  USING      (auth_subject = current_setting('app.user_subject', true))
  WITH CHECK (auth_subject = current_setting('app.user_subject', true));

-- The normalizer's read path. SECURITY DEFINER because the caller is a tenant
-- role that must not be able to select from ingress_items directly. It returns
-- only rows already bound to the caller's tenant, and it never returns
-- unverified items -- accepting an unverified payload would defeat the point
-- of verifying the HMAC.
CREATE OR REPLACE FUNCTION app_next_ingress_batch(p_limit int DEFAULT 100)
RETURNS SETOF ingress_items
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
BEGIN
  RETURN QUERY
    UPDATE ingress_items i
       SET normalized_at = now()
     WHERE i.id IN (
             SELECT id FROM ingress_items
              WHERE signature_verified
                AND normalized_at IS NULL
                AND tenant_id = app_current_tenant()
                AND quarantine_reason IS NULL
              ORDER BY received_at, id
              LIMIT p_limit
              FOR UPDATE SKIP LOCKED
           )
    RETURNING i.*;
END
$$;

REVOKE ALL ON FUNCTION app_next_ingress_batch(int) FROM PUBLIC;

-- Granted here rather than in 0010 because the function is SECURITY DEFINER:
-- its callers deliberately cannot reach ingress_items directly, so the grant is
-- part of the same decision as the function body above.
GRANT EXECUTE ON FUNCTION app_next_ingress_batch(int) TO app_runtime;
