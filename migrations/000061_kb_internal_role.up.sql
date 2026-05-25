-- Create a non-BYPASSRLS role for kb_internal reads/writes.
--
-- Why: the default Neon role (neondb_owner) has rolbypassrls=true, which
-- defeats `FORCE ROW LEVEL SECURITY`. The GoClaw KB store must SET LOCAL ROLE
-- to this role before touching kb_internal.* so that the RLS policies in
-- 000059 + 000060 actually fire.
--
-- The role has no LOGIN — it's only ever assumed via SET LOCAL ROLE from the
-- pooled owner connection. The GRANT TO statement covers any role we expect
-- to connect (idempotent and safe to re-run).

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'kb_internal_user') THEN
        CREATE ROLE kb_internal_user NOLOGIN NOBYPASSRLS;
    END IF;
END $$;

-- Grant membership to the current connection role so SET LOCAL ROLE works.
-- We only touch current_user — granting to every role would hit Neon's
-- protected system roles (cloud_admin, etc.) and fail. Add additional GRANTs
-- here if other connection roles are introduced later.
DO $$
BEGIN
    EXECUTE format('GRANT kb_internal_user TO %I', current_user);
END $$;

GRANT USAGE ON SCHEMA kb_internal TO kb_internal_user;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA kb_internal TO kb_internal_user;

-- Ensure future tables in kb_internal grant to the role automatically.
ALTER DEFAULT PRIVILEGES IN SCHEMA kb_internal
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO kb_internal_user;
