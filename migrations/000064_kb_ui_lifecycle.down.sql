-- Reverse 000064. Drops kb_quarantine entirely; reverts kb_documents / kb_ingest_runs columns.

DROP SCHEMA IF EXISTS kb_quarantine CASCADE;

DROP INDEX IF EXISTS kb.kb_internal_documents_tenant_status;
DROP INDEX IF EXISTS kb.kb_documents_tenant_status;

ALTER TABLE kb.kb_ingest_runs DROP CONSTRAINT IF EXISTS kb_ingest_runs_status_check;
ALTER TABLE kb.kb_ingest_runs ADD CONSTRAINT kb_ingest_runs_status_check
  CHECK (status IN ('running','ok','failed'));

ALTER TABLE kb.kb_ingest_runs
  DROP COLUMN IF EXISTS log_url,
  DROP COLUMN IF EXISTS stats,
  DROP COLUMN IF EXISTS kind;

ALTER TABLE kb_internal.kb_documents
  DROP COLUMN IF EXISTS source_kind,
  DROP COLUMN IF EXISTS updated_at,
  DROP COLUMN IF EXISTS uploaded_by,
  DROP COLUMN IF EXISTS error,
  DROP COLUMN IF EXISTS status;

ALTER TABLE kb.kb_documents
  DROP COLUMN IF EXISTS source_kind,
  DROP COLUMN IF EXISTS updated_at,
  DROP COLUMN IF EXISTS uploaded_by,
  DROP COLUMN IF EXISTS error,
  DROP COLUMN IF EXISTS status;
