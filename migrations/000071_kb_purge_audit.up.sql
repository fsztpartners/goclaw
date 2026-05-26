-- Phase 8 P8.4 — GDPR / customer-churn purge audit log.
--
-- Records every /v1/kb/purge call so a deleted tenant leaves a forensic
-- trail (who, when, what, how many rows). Intentionally NOT FK-referenced
-- to tenants(id): the audit must survive the tenant row being deleted.
--
-- The actual cascading delete of kb.*/kb_internal.*/kb_quarantine.* is driven
-- by the application code in internal/kb/purge.go (NOT a tenants ON DELETE
-- CASCADE), because GDPR purge keeps the tenant row but nukes its data —
-- so cascade-from-tenant is the wrong shape.

CREATE TABLE IF NOT EXISTS kb.purge_audit (
    id                    UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
    tenant_id             UUID NOT NULL,          -- intentional: no FK, audit must survive
    purged_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    purged_by             TEXT NOT NULL,          -- GOCLAW_USER_ID or 'system'
    reason                TEXT,                   -- 'gdpr_request' | 'tenant_churn' | 'test'
    -- Per-table counts so an auditor can verify the cross-table cleanup
    -- summed to what we expected at purge time.
    counts_json           JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- Free-form: subject email if GDPR, contact identifier, request ticket
    requester             TEXT,
    -- Latency to complete the full multi-schema delete.
    duration_ms           BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS kb_purge_audit_tenant_ts
    ON kb.purge_audit (tenant_id, purged_at DESC);
