ALTER DEFAULT PRIVILEGES IN SCHEMA kb_internal
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM kb_internal_user;

REVOKE ALL ON ALL TABLES IN SCHEMA kb_internal FROM kb_internal_user;
REVOKE USAGE ON SCHEMA kb_internal FROM kb_internal_user;

DROP ROLE IF EXISTS kb_internal_user;
