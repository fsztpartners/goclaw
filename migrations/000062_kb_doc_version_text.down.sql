ALTER TABLE kb.kb_documents          ALTER COLUMN doc_version DROP DEFAULT;
ALTER TABLE kb.kb_chunks             ALTER COLUMN doc_version DROP DEFAULT;
ALTER TABLE kb_internal.kb_documents ALTER COLUMN doc_version DROP DEFAULT;
ALTER TABLE kb_internal.kb_chunks    ALTER COLUMN doc_version DROP DEFAULT;

ALTER TABLE kb.kb_documents          ALTER COLUMN doc_version TYPE INT USING doc_version::INT;
ALTER TABLE kb.kb_chunks             ALTER COLUMN doc_version TYPE INT USING doc_version::INT;
ALTER TABLE kb_internal.kb_documents ALTER COLUMN doc_version TYPE INT USING doc_version::INT;
ALTER TABLE kb_internal.kb_chunks    ALTER COLUMN doc_version TYPE INT USING doc_version::INT;

ALTER TABLE kb.kb_documents          ALTER COLUMN doc_version SET DEFAULT 1;
ALTER TABLE kb.kb_chunks             ALTER COLUMN doc_version SET DEFAULT 1;
ALTER TABLE kb_internal.kb_documents ALTER COLUMN doc_version SET DEFAULT 1;
ALTER TABLE kb_internal.kb_chunks    ALTER COLUMN doc_version SET DEFAULT 1;
