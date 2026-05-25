DROP POLICY IF EXISTS kb_internal_chunks_role_filter   ON kb_internal.kb_chunks;
DROP POLICY IF EXISTS kb_internal_chunks_tenant_iso    ON kb_internal.kb_chunks;
DROP POLICY IF EXISTS kb_internal_documents_tenant_iso ON kb_internal.kb_documents;

ALTER TABLE kb_internal.kb_chunks    DISABLE ROW LEVEL SECURITY;
ALTER TABLE kb_internal.kb_documents DISABLE ROW LEVEL SECURITY;
