DROP POLICY IF EXISTS retrieval_audit_tenant_iso ON kb_internal.retrieval_audit;
DROP INDEX IF EXISTS kb_internal.retrieval_audit_agent_idx;
DROP INDEX IF EXISTS kb_internal.retrieval_audit_tenant_created_idx;
DROP TABLE IF EXISTS kb_internal.retrieval_audit;
