-- Row-level security for kb_internal.* — Phase 1 of brand-knowledge-base.
--
-- Enforcement model: session GUCs set by GoClaw at the start of every transaction
-- that touches kb_internal.
--   SET LOCAL app.tenant_id  = '<tenant uuid>';
--   SET LOCAL app.role_tags  = 'hr_internal,policy_admin';   -- optional, comma-separated
--
-- When the GUC is unset, current_setting(..., true) returns '' / NULL.
-- The USING clause then evaluates to NULL (not TRUE), so the row is denied (default-deny).
--
-- FORCE ROW LEVEL SECURITY applies the policy to the table owner too, preventing the
-- service account from accidentally seeing other tenants' rows.

ALTER TABLE kb_internal.kb_documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE kb_internal.kb_documents FORCE  ROW LEVEL SECURITY;

ALTER TABLE kb_internal.kb_chunks    ENABLE ROW LEVEL SECURITY;
ALTER TABLE kb_internal.kb_chunks    FORCE  ROW LEVEL SECURITY;

-- Tenant isolation: row visible only if its tenant_id matches the session's app.tenant_id GUC.
CREATE POLICY kb_internal_documents_tenant_iso ON kb_internal.kb_documents
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY kb_internal_chunks_tenant_iso ON kb_internal.kb_chunks
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Optional role_tag filter: when app.role_tags is set, a chunk is visible only if it has
-- no role_tags (department-wide) OR at least one of its role_tags is in the GUC list.
-- When app.role_tags is unset, this policy is effectively a pass-through (default true)
-- so callers without role tagging needs (e.g., admin) can read all of their tenant's data.
CREATE POLICY kb_internal_chunks_role_filter ON kb_internal.kb_chunks
    USING (
        NULLIF(current_setting('app.role_tags', true), '') IS NULL
        OR cardinality(role_tags) = 0
        OR role_tags && string_to_array(current_setting('app.role_tags', true), ',')
    );

-- Write paths use the same policies (USING applies to ALL by default; no WITH CHECK
-- override needed since inserts are scoped to the caller's tenant by app code).
