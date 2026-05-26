DROP TABLE IF EXISTS kb.kg_extract_jobs;
DROP TRIGGER IF EXISTS kb_kg_entity_chunks_tenant_check ON kb.kg_entity_chunks;
DROP FUNCTION IF EXISTS kb.kg_entity_chunks_tenant_check();
DROP TABLE IF EXISTS kb.kg_entity_chunks;
DROP TRIGGER IF EXISTS kb_kg_relations_tenant_check ON kb.kg_relations;
DROP FUNCTION IF EXISTS kb.kg_relations_tenant_check();
DROP TABLE IF EXISTS kb.kg_relations;
DROP TABLE IF EXISTS kb.kg_entities;
