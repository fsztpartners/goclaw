-- Phase 4 — Ingestion UI lifecycle.
-- Adds status / error / uploaded_by / updated_at / source_kind columns to kb_documents
-- (and kb_internal.kb_documents) so the new admin UI can render per-doc lifecycle.
-- Adds kind / stats / log_url to kb_ingest_runs so the runs timeline can show progress.
-- Creates kb_quarantine schema for sandboxed customer uploads (P4.6 — hardening in Phase 7).
--
-- IMPORTANT: this is a metadata-only migration. Existing chunks are not re-ingested;
-- existing kb_documents rows are back-filled with status='indexed' (they have chunks)
-- and source_kind defaulted from source_kind_meta.
--
-- Tenants are NOT re-modeled: the existing public `tenants` table (000027) IS the brand
-- registry per Phase 1 carry-forward. KB-specific defaults (default_dept, default_visibility)
-- live in tenants.settings->'kb' so we don't fork the schema.

-- ============================================================
-- kb.kb_documents — add lifecycle columns
-- ============================================================

ALTER TABLE kb.kb_documents
  ADD COLUMN IF NOT EXISTS status      TEXT NOT NULL DEFAULT 'indexed'
    CHECK (status IN ('queued','chunking','embedding','indexed','failed','superseded')),
  ADD COLUMN IF NOT EXISTS error       TEXT,
  ADD COLUMN IF NOT EXISTS uploaded_by TEXT,
  ADD COLUMN IF NOT EXISTS updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  ADD COLUMN IF NOT EXISTS source_kind TEXT;

-- back-fill source_kind from the existing source_kind_meta (same value, new name for UI clarity)
UPDATE kb.kb_documents SET source_kind = source_kind_meta WHERE source_kind IS NULL;

-- back-fill status: if the doc has chunks, mark it indexed; otherwise leave default
UPDATE kb.kb_documents d SET status = 'indexed'
  WHERE EXISTS (SELECT 1 FROM kb.kb_chunks c WHERE c.doc_id = d.id);

CREATE INDEX IF NOT EXISTS kb_documents_tenant_status ON kb.kb_documents(tenant_id, status);

ALTER TABLE kb_internal.kb_documents
  ADD COLUMN IF NOT EXISTS status      TEXT NOT NULL DEFAULT 'indexed'
    CHECK (status IN ('queued','chunking','embedding','indexed','failed','superseded')),
  ADD COLUMN IF NOT EXISTS error       TEXT,
  ADD COLUMN IF NOT EXISTS uploaded_by TEXT,
  ADD COLUMN IF NOT EXISTS updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  ADD COLUMN IF NOT EXISTS source_kind TEXT;

UPDATE kb_internal.kb_documents SET source_kind = source_kind_meta WHERE source_kind IS NULL;
UPDATE kb_internal.kb_documents d SET status = 'indexed'
  WHERE EXISTS (SELECT 1 FROM kb_internal.kb_chunks c WHERE c.doc_id = d.id);

CREATE INDEX IF NOT EXISTS kb_internal_documents_tenant_status ON kb_internal.kb_documents(tenant_id, status);

-- ============================================================
-- kb.kb_ingest_runs — add kind / stats / log_url and relax status enum
-- ============================================================

ALTER TABLE kb.kb_ingest_runs
  ADD COLUMN IF NOT EXISTS kind    TEXT,
  ADD COLUMN IF NOT EXISTS stats   JSONB NOT NULL DEFAULT '{}'::jsonb,
  ADD COLUMN IF NOT EXISTS log_url TEXT;

-- The existing status CHECK was ('running','ok','failed'). The UI also wants 'queued'.
ALTER TABLE kb.kb_ingest_runs DROP CONSTRAINT IF EXISTS kb_ingest_runs_status_check;
ALTER TABLE kb.kb_ingest_runs ADD CONSTRAINT kb_ingest_runs_status_check
  CHECK (status IN ('queued','running','ok','failed'));

-- ============================================================
-- kb_quarantine schema — sandboxed customer uploads (P4.6)
--
-- Same table shape as kb.* but isolated. provenance_trust forced to 'customer'.
-- "Promote" copies rows into kb.*; "Reject" deletes from kb_quarantine.* only.
-- Phase 7 adds RLS + dedicated user; Phase 4 ships the surface.
-- ============================================================

CREATE SCHEMA IF NOT EXISTS kb_quarantine;

CREATE TABLE IF NOT EXISTS kb_quarantine.kb_documents (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
    tenant_id        UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    department       TEXT NOT NULL,
    visibility       TEXT NOT NULL DEFAULT 'public' CHECK (visibility = 'public'),
    title            TEXT,
    source_uri       TEXT,
    source_kind_meta TEXT NOT NULL CHECK (source_kind_meta IN ('pdf','pdf_visual','video','web','upload')),
    source_kind      TEXT,
    doc_hash         VARCHAR(64) NOT NULL,
    doc_version      TEXT NOT NULL DEFAULT '1',
    status           TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued','chunking','embedding','quarantined','promoted','rejected','failed')),
    error            TEXT,
    uploaded_by      TEXT,
    ingested_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    superseded_at    TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS kb_quarantine_documents_tenant_hash ON kb_quarantine.kb_documents(tenant_id, doc_hash);
CREATE INDEX IF NOT EXISTS kb_quarantine_documents_tenant_status ON kb_quarantine.kb_documents(tenant_id, status);

CREATE TABLE IF NOT EXISTS kb_quarantine.kb_chunks (
    id                   UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
    tenant_id            UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    doc_id               UUID NOT NULL REFERENCES kb_quarantine.kb_documents(id) ON DELETE CASCADE,
    department           TEXT NOT NULL,
    visibility           TEXT NOT NULL DEFAULT 'public' CHECK (visibility = 'public'),
    provenance_trust     TEXT NOT NULL DEFAULT 'customer' CHECK (provenance_trust = 'customer'),
    role_tags            TEXT[] NOT NULL DEFAULT '{}',
    doc_hash             VARCHAR(64) NOT NULL,
    doc_version          TEXT NOT NULL DEFAULT '1',
    chunk_idx            INT NOT NULL,
    page                 INT,
    timestamp_sec        REAL,
    embedding_model_id   TEXT NOT NULL,
    embedding_adapter_id TEXT,
    embedding            vector(1024) NOT NULL,
    text                 TEXT NOT NULL,
    ingested_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    pipeline_version     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS kb_quarantine_chunks_doc ON kb_quarantine.kb_chunks(doc_id);
CREATE INDEX IF NOT EXISTS kb_quarantine_chunks_tenant ON kb_quarantine.kb_chunks(tenant_id);

COMMENT ON SCHEMA kb_quarantine IS
  'Phase 4 customer-upload sandbox. Promote-or-reject from /admin/knowledge/sources. Phase 7 hardens with RLS + dedicated user.';
COMMENT ON COLUMN kb.kb_documents.status IS
  'Lifecycle: queued -> chunking -> embedding -> indexed (terminal). superseded set on soft-delete. failed terminal on error.';
COMMENT ON COLUMN kb.kb_ingest_runs.kind IS
  'Source kind that produced this run: pdf | markdown | paste | url | crawl | video | visual_pdf | upload | reembed.';
COMMENT ON COLUMN kb.kb_ingest_runs.stats IS
  'Progress events + final stats: {step_events:[...], chunk_count, token_count, cost_usd_breakdown}.';
