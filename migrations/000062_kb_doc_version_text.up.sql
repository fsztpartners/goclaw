-- Convert doc_version from INT to TEXT so it can store git SHAs / version strings
-- directly. Phase 1 was hashing SHAs down to a 31-bit int via a client-side helper;
-- this lets callers store the SHA as-is.
--
-- Safe migration: every existing row's int gets cast to text (the digit string).
-- Going back (down migration) requires the value to be a parseable integer.

ALTER TABLE kb.kb_documents          ALTER COLUMN doc_version TYPE TEXT USING doc_version::TEXT;
ALTER TABLE kb.kb_chunks             ALTER COLUMN doc_version TYPE TEXT USING doc_version::TEXT;
ALTER TABLE kb_internal.kb_documents ALTER COLUMN doc_version TYPE TEXT USING doc_version::TEXT;
ALTER TABLE kb_internal.kb_chunks    ALTER COLUMN doc_version TYPE TEXT USING doc_version::TEXT;

-- Defaults stay numeric for now (DEFAULT 1::TEXT = '1') — callers that pass nothing
-- still work, callers that pass git SHAs now get them stored verbatim.
ALTER TABLE kb.kb_documents          ALTER COLUMN doc_version SET DEFAULT '1';
ALTER TABLE kb.kb_chunks             ALTER COLUMN doc_version SET DEFAULT '1';
ALTER TABLE kb_internal.kb_documents ALTER COLUMN doc_version SET DEFAULT '1';
ALTER TABLE kb_internal.kb_chunks    ALTER COLUMN doc_version SET DEFAULT '1';
