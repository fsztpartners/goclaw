-- Phase 6-real (R6.8) — adapter promotion audit table.
--
-- Records every LoRA adapter that the trivia loop trained and (optionally)
-- promoted. Promotion = "the tenant's default embedding_model_id flips to
-- this adapter's id for new chunk ingests". The actual chunk rows live in
-- kb.kb_chunks under (embedding_model_id, embedding_adapter_id); this table
-- is the audit log + the source of truth for which adapter is currently
-- promoted per tenant.
--
-- Storage: adapter .safetensors files live on object storage (or the GPU pod's
-- volume during dev); this row holds the *URI*, not the bytes. At 50MB ×
-- 100 brands × 10 versions = 50GB long-term — out of band of Postgres.

CREATE TABLE kb.embedding_adapters (
    id                    UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
    tenant_id             UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    -- Stored on every chunk under embedding_model_id = base_model + '+adapter:' + adapter_id.
    -- adapter_id is a human-readable slug like 'gitlab-2026-05-22'.
    adapter_id            TEXT NOT NULL,
    base_model            TEXT NOT NULL,                       -- e.g. 'qwen3-embed-4b@1024'
    -- Where the .safetensors lives (s3://… or pod-local file:// during dev).
    adapter_path          TEXT NOT NULL,
    -- Where the CustomIR-shape JSONL the miner produced lives.
    training_jsonl_uri    TEXT,
    -- Path to the A/B JSON report (eval/reports/phase6-*.json).
    ab_report_uri         TEXT,
    -- Quality-floor gate result. NULL means it was never run.
    quality_floor_pass    BOOLEAN,
    -- Adapter file metadata.
    size_bytes            BIGINT,
    sha256                TEXT,
    -- Promotion lifecycle.
    promoted_at           TIMESTAMPTZ,
    promoted_by           TEXT,                                -- GOCLAW_USER_ID at time of promotion
    rejected_at           TIMESTAMPTZ,
    rejected_reason       TEXT,
    -- Free-form notes (hyperparam summary, dataset version, etc.).
    notes                 TEXT,
    -- Carry-forward summary (recall@5 base vs candidate, regressions count).
    metrics_json          JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Human-friendly lookup by tenant+adapter_id. Hard UNIQUE so the same
-- adapter_id slug is never written twice under one tenant.
CREATE UNIQUE INDEX embedding_adapters_tenant_adapter_id_idx
    ON kb.embedding_adapters (tenant_id, adapter_id);

-- Only one promoted adapter per (tenant, base_model) at a time. Enforced via
-- a partial unique index; rejected/in-flight rows don't constrain.
CREATE UNIQUE INDEX embedding_adapters_one_promoted_per_tenant_idx
    ON kb.embedding_adapters (tenant_id, base_model)
    WHERE promoted_at IS NOT NULL AND rejected_at IS NULL;

CREATE INDEX embedding_adapters_tenant_idx ON kb.embedding_adapters (tenant_id);

-- updated_at trigger.
CREATE OR REPLACE FUNCTION kb.embedding_adapters_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER embedding_adapters_set_updated_at_trg
    BEFORE UPDATE ON kb.embedding_adapters
    FOR EACH ROW
    EXECUTE FUNCTION kb.embedding_adapters_set_updated_at();
