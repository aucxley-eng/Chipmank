-- 0010_roles.sql
-- Runtime roles and the connection-scoping contract.
--
-- Runs before the RLS migration on purpose: the policies in 0011 are declared
-- FOR specific roles, so the roles have to exist first. Roles are created
-- before policies rather than after, because a policy naming a missing role is
-- a hard error and would otherwise leave the migration half-applied.
--
-- v2 section 30: "Use non-owner application roles without BYPASSRLS; FORCE ROW
-- LEVEL SECURITY where appropriate. Set tenant context locally inside a
-- transaction and reset pooled connections."
--
-- Every runtime role below is explicitly checked for BYPASSRLS and SUPERUSER
-- at creation time, so a future edit that grants them is caught by the
-- isolation test rather than shipping.

-- Migration role: owns the schema, never used by the application at runtime.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_migrator') THEN
    CREATE ROLE app_migrator NOLOGIN;
  END IF;
END
$$;
GRANT ALL ON ALL TABLES IN SCHEMA public TO app_migrator;
GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO app_migrator;

-- Application role. LOGIN, no inheritance of ownership, no BYPASSRLS.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_runtime') THEN
    CREATE ROLE app_runtime LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE
                     NOINHERIT NOBYPASSRLS;
  END IF;
END
$$;
GRANT USAGE ON SCHEMA public TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_runtime;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_runtime;

-- The claim/complete helpers are SECURITY INVOKER, so app_runtime still runs
-- under the tenant policy while calling them. Nothing here bypasses RLS.
-- app_next_ingress_batch is granted in 0011, next to its definition, because
-- it is SECURITY DEFINER and its access rules are part of that function.
GRANT EXECUTE ON FUNCTION app_claim_jobs(text, int, int) TO app_runtime;
GRANT EXECUTE ON FUNCTION app_reclaim_expired_jobs() TO app_runtime;
GRANT EXECUTE ON FUNCTION app_current_tenant() TO app_runtime;
GRANT EXECUTE ON FUNCTION app_delivery_rank(text) TO app_runtime;

-- Reporting role: read-only, still tenant-filtered.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_readonly') THEN
    CREATE ROLE app_readonly LOGIN NOSUPERUSER NOINHERIT NOBYPASSRLS;
  END IF;
END
$$;
GRANT USAGE ON SCHEMA public TO app_readonly;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO app_readonly;
GRANT EXECUTE ON FUNCTION app_current_tenant() TO app_readonly;

-- ------------------------------------------------------- privilege guardrails
-- app_runtime is a shared role across tenants; the tenant boundary comes from
-- RLS plus app.tenant_id, not from a per-tenant role. That is a deliberate
-- choice: per-tenant roles do not scale past a few hundred tenants.
--
-- The tables the runtime role must never touch are revoked explicitly, so a
-- broad GRANT above cannot leak into them.
REVOKE ALL ON ingress_items          FROM app_runtime, app_readonly;
REVOKE ALL ON ingress_receipts       FROM app_runtime, app_readonly;
REVOKE ALL ON ingress_rejections     FROM app_runtime, app_readonly;
REVOKE ALL ON tenant_routing_bindings FROM app_runtime, app_readonly;

-- The ingress service needs a dedicated role. It is the ONLY runtime role that
-- can insert into the durable-acceptance tables, and it cannot read any
-- tenant business data. It also cannot SELECT from ingress_items: the append
-- path only needs INSERT, and read-back of raw inbound bodies is a data leak
-- waiting to happen. Reading for normalisation goes through the SECURITY
-- DEFINER function instead.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_ingress') THEN
    CREATE ROLE app_ingress LOGIN NOSUPERUSER NOINHERIT NOBYPASSRLS;
  END IF;
END
$$;
GRANT USAGE ON SCHEMA public TO app_ingress;
GRANT INSERT                ON ingress_items          TO app_ingress;
GRANT INSERT                ON ingress_rejections     TO app_ingress;
GRANT INSERT                ON ingress_receipts       TO app_ingress;
GRANT SELECT                ON tenant_routing_bindings TO app_ingress;
GRANT EXECUTE ON FUNCTION app_current_tenant()      TO app_ingress;

-- The three ingress tables use bigserial, so INSERT also needs USAGE on their
-- sequences. The sequence is not a cosmetic detail here: it is the monotonic
-- log position a status receipt points back at, so it must be a real serial.
-- Only these three are granted, not ALL SEQUENCES, so a role added later cannot
-- quietly draw ids from unrelated tables.
GRANT USAGE ON SEQUENCE ingress_items_id_seq      TO app_ingress;
GRANT USAGE ON SEQUENCE ingress_receipts_id_seq   TO app_ingress;
GRANT USAGE ON SEQUENCE ingress_rejections_id_seq TO app_ingress;

-- ----------------------------------------------------- connection discipline
-- Every transaction that touches tenant data must set the scope, and must not
-- inherit one from a pooled connection. These helpers make the correct
-- sequence a single call.
CREATE OR REPLACE FUNCTION app_set_tenant(p_tenant uuid) RETURNS void
LANGUAGE sql AS $$
  SELECT set_config('app.tenant_id', p_tenant::text, true)
$$;

-- One-statement transaction wrapper: set the scope, run the body, and clear it
-- on the way out regardless of outcome. Callers use this instead of trusting
-- a manual SET/RESET pair.
CREATE OR REPLACE FUNCTION app_with_tenant(p_tenant uuid, p_sql text)
RETURNS void
LANGUAGE plpgsql
AS $$
BEGIN
  PERFORM set_config('app.tenant_id', p_tenant::text, true);
  EXECUTE p_sql;
EXCEPTION WHEN OTHERS THEN
  -- Leave app.tenant_id cleared; the caller's transaction is already doomed
  -- but a pooled connection must not carry a stale scope outward.
  PERFORM set_config('app.tenant_id', '', true);
  RAISE;
END
$$;

REVOKE ALL ON FUNCTION app_with_tenant(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_set_tenant(uuid) TO app_runtime, app_readonly;

-- ------------------------------------------------------- startup assertions
-- Fail loudly at deploy time if a runtime role has been given a privilege that
-- would silently defeat isolation. v2 section 39 makes tenant isolation a
-- launch gate, so this is a hard error, not a warning.
CREATE OR REPLACE FUNCTION app_assert_isolation_principles() RETURNS void
LANGUAGE plpgsql AS $$
DECLARE bad text;
BEGIN
  SELECT string_agg(rolname, ', ')
    INTO bad
    FROM pg_roles
   WHERE rolname IN ('app_runtime', 'app_readonly', 'app_ingress')
     AND (rolbypassrls OR rolsuper);

  IF bad IS NOT NULL THEN
    RAISE EXCEPTION
      'tenant isolation violated: role(s) % have BYPASSRLS or SUPERUSER', bad;
  END IF;
END
$$;
