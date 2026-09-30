-- 0002_identity.sql
-- Tenants, users, memberships, teams.
--
-- v2 section 6 models an agency organization above child workspaces. For the
-- first slice we ship the flat tenant only; organization_id / kind / parent_id
-- land in a later migration so the isolation boundary is already correct and
-- adding the hierarchy does not rewrite these tables.

CREATE TABLE tenants (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  slug            text NOT NULL,
  name            text   NOT NULL,
  status          text   NOT NULL DEFAULT 'active'
                  CHECK (status IN ('provisioning', 'active', 'suspended', 'closed')),
  home_cell       text   NOT NULL DEFAULT 'cell_1',
  retention_days  int    NOT NULL DEFAULT 90 CHECK (retention_days > 0),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER tenants_touch BEFORE UPDATE ON tenants
  FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();

-- Case-insensitive slug uniqueness, without citext.
CREATE UNIQUE INDEX tenants_slug_lower_idx ON tenants (lower(slug));

-- Global identity is deliberately NOT tenant-scoped: one person may hold
-- memberships in several tenants. v2 section 30 keeps tenant membership
-- separate from global identity for exactly this reason.
CREATE TABLE users (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  auth_subject     text NOT NULL UNIQUE,     -- IdP subject claim
  email            text NOT NULL,
  display_name     text,
  mfa_enrolled     boolean NOT NULL DEFAULT false,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER users_touch BEFORE UPDATE ON users
  FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();

-- One account per email regardless of case. auth_subject is the real identity
-- key; this is a data-integrity guard, not an auth mechanism.
CREATE UNIQUE INDEX users_email_lower_idx ON users (lower(email));

CREATE TABLE teams (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name        text NOT NULL,
  -- v2 section 16: routing considers language, skills, working hours.
  language    text,
  timezone    text NOT NULL DEFAULT 'UTC',
  working_hours jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, name)
);
CREATE INDEX teams_tenant_idx ON teams (tenant_id);
CREATE TRIGGER teams_touch BEFORE UPDATE ON teams
  FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();

-- v2 section 30 role set. 'owner' and 'admin' are tenant-wide; 'agent' and
-- 'supervisor' are additionally constrained by team membership at query time.
CREATE TABLE tenant_memberships (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role        text NOT NULL
              CHECK (role IN ('owner','admin','supervisor','agent','analyst','developer','billing')),
  team_id     uuid REFERENCES teams(id) ON DELETE SET NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, user_id, team_id)
);
CREATE INDEX memberships_tenant_idx ON tenant_memberships (tenant_id, user_id);
CREATE TRIGGER memberships_touch BEFORE UPDATE ON tenant_memberships
  FOR EACH ROW EXECUTE FUNCTION app_touch_updated_at();
