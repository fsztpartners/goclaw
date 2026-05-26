DROP POLICY IF EXISTS kb_internal_chunks_role_filter ON kb_internal.kb_chunks;

CREATE POLICY kb_internal_chunks_role_filter ON kb_internal.kb_chunks
    USING (
        NULLIF(current_setting('app.role_tags', true), '') IS NULL
        OR cardinality(role_tags) = 0
        OR role_tags && string_to_array(current_setting('app.role_tags', true), ',')
    );
