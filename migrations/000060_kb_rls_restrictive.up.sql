-- Fix RLS policy combination: the role_filter policy needs to be RESTRICTIVE
-- so that both tenant_iso AND role_filter must pass. With the original
-- PERMISSIVE-by-default semantics, the policies were OR'd — and when
-- app.role_tags was unset, role_filter permitted every row, defeating
-- tenant isolation.
--
-- Bug discovered during Phase 1 RLS smoke; see learnings.md.

DROP POLICY IF EXISTS kb_internal_chunks_role_filter ON kb_internal.kb_chunks;

CREATE POLICY kb_internal_chunks_role_filter ON kb_internal.kb_chunks
    AS RESTRICTIVE
    USING (
        NULLIF(current_setting('app.role_tags', true), '') IS NULL
        OR cardinality(role_tags) = 0
        OR role_tags && string_to_array(current_setting('app.role_tags', true), ',')
    );
