-- Phase 7 P7.9 — RLS + dedicated role for kb_quarantine schema.
--
-- Today (post-Phase 4 P4.6) kb_quarantine has the same shape as kb (kb_documents
-- + kb_chunks) but no RLS, and only neondb_owner has access. That means any
-- code path with the default connection can read every quarantined upload from
-- every tenant. P7.9 mirrors the kb_internal isolation pattern:
--   - Dedicated NOLOGIN role kb_quarantine_user, granted to current_user via
--     SET LOCAL ROLE.
--   - FORCE ROW LEVEL SECURITY on both tables, tenant-scoped via app.tenant_id GUC.
--   - PERMISSIVE tenant_iso policies.
--
-- Promote-to-kb operations still happen via neondb_owner (so they can write
-- across schemas), but the read/write surface for the operator UI is locked
-- down via SET LOCAL ROLE before any quarantine-listing query.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'kb_quarantine_user') THEN
        CREATE ROLE kb_quarantine_user NOLOGIN NOBYPASSRLS;
    END IF;
END $$;

DO $$
BEGIN
    EXECUTE format('GRANT kb_quarantine_user TO %I', current_user);
END $$;

GRANT USAGE ON SCHEMA kb_quarantine TO kb_quarantine_user;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA kb_quarantine TO kb_quarantine_user;
ALTER DEFAULT PRIVILEGES IN SCHEMA kb_quarantine
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO kb_quarantine_user;

-- kb_quarantine.kb_documents — tenant isolation
ALTER TABLE kb_quarantine.kb_documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE kb_quarantine.kb_documents FORCE ROW LEVEL SECURITY;
CREATE POLICY kb_quarantine_documents_tenant_iso
    ON kb_quarantine.kb_documents
    FOR ALL TO kb_quarantine_user
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- kb_quarantine.kb_chunks — tenant isolation
ALTER TABLE kb_quarantine.kb_chunks ENABLE ROW LEVEL SECURITY;
ALTER TABLE kb_quarantine.kb_chunks FORCE ROW LEVEL SECURITY;
CREATE POLICY kb_quarantine_chunks_tenant_iso
    ON kb_quarantine.kb_chunks
    FOR ALL TO kb_quarantine_user
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
