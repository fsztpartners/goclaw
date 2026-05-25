-- Phase 7 P7.8 — audit log for every kb_internal retrieval.
--
-- Plan invariant: "audit log: every internal-doc retrieval logged with
-- agent_id + query + chunk_ids". This table is the source of truth for
-- internal-data access. retrieval_traces (Phase 2 P2.6) is a general telemetry
-- table — it lives in the public schema and isn't required to record every
-- internal hit. kb_internal_retrieval_audit IS required.
--
-- Lives in kb_internal so a single bulk DELETE of a tenant's internal data
-- (GDPR purge in Phase 8 P8.7) also drops the audit trail.

CREATE TABLE kb_internal.retrieval_audit (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    -- Agent that requested the data. Either a user-facing agent_id (e.g.
    -- hr-internal-bot) or 'fzst-claw-system' for offline workflows.
    agent_id        TEXT NOT NULL,
    -- The raw query text the agent sent. PII-bearing — same retention rules
    -- as the chunks themselves (cascade on tenant delete).
    query           TEXT NOT NULL,
    -- Role + role_tags that were asserted on the retrieve call. The
    -- request-side enforcement (schemaFor("internal")) means only role values
    -- that map to internal land here. A 'hr_recruiter' (public) row in this
    -- table indicates a misuse and should alert.
    role            TEXT NOT NULL,
    role_tags       TEXT[] NOT NULL DEFAULT '{}'::text[],
    -- Which chunk_ids were returned. nullable for the "refused / 0 results" case.
    chunk_ids       UUID[] NOT NULL DEFAULT '{}'::uuid[],
    -- Route + reranker decision for the trace.
    route           TEXT,
    rerank_provider TEXT,
    -- Server-measured latency.
    latency_ms      INT,
    -- Anything else worth keeping (e.g. classifier output, route_override).
    extra           JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX retrieval_audit_tenant_created_idx
    ON kb_internal.retrieval_audit (tenant_id, created_at DESC);
CREATE INDEX retrieval_audit_agent_idx
    ON kb_internal.retrieval_audit (agent_id, created_at DESC);

-- Audit table is read-only for kb_internal_user (writes go through the same
-- pooled-owner connection that handles ingest writes; reads against the audit
-- table need to bypass the role-filter RESTRICTIVE policy we add for kb_chunks).
GRANT SELECT, INSERT ON kb_internal.retrieval_audit TO kb_internal_user;

ALTER TABLE kb_internal.retrieval_audit ENABLE ROW LEVEL SECURITY;
ALTER TABLE kb_internal.retrieval_audit FORCE ROW LEVEL SECURITY;

-- Tenant isolation — same shape as kb_internal.kb_chunks_tenant_iso.
CREATE POLICY retrieval_audit_tenant_iso
    ON kb_internal.retrieval_audit
    FOR ALL TO kb_internal_user
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
