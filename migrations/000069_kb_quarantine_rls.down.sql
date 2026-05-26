DROP POLICY IF EXISTS kb_quarantine_chunks_tenant_iso ON kb_quarantine.kb_chunks;
ALTER TABLE kb_quarantine.kb_chunks NO FORCE ROW LEVEL SECURITY;
ALTER TABLE kb_quarantine.kb_chunks DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS kb_quarantine_documents_tenant_iso ON kb_quarantine.kb_documents;
ALTER TABLE kb_quarantine.kb_documents NO FORCE ROW LEVEL SECURITY;
ALTER TABLE kb_quarantine.kb_documents DISABLE ROW LEVEL SECURITY;
REVOKE ALL ON ALL TABLES IN SCHEMA kb_quarantine FROM kb_quarantine_user;
REVOKE USAGE ON SCHEMA kb_quarantine FROM kb_quarantine_user;
ALTER DEFAULT PRIVILEGES IN SCHEMA kb_quarantine
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM kb_quarantine_user;
-- Role is intentionally NOT dropped; other migrations may still reference it.
