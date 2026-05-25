DROP TRIGGER IF EXISTS pageindex_trees_set_updated_at_trg ON kb_internal.pageindex_trees;
DROP FUNCTION IF EXISTS kb_internal.pageindex_trees_set_updated_at();
DROP POLICY IF EXISTS pageindex_trees_tenant_iso ON kb_internal.pageindex_trees;
DROP INDEX IF EXISTS kb_internal.pageindex_trees_tenant_name_idx;
DROP TABLE IF EXISTS kb_internal.pageindex_trees;
