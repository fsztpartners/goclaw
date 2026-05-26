-- Phase 2 — JWT verifier configuration.
-- Single-row table holding the JWKS URL, issuer, and audience that the
-- gateway uses to verify access tokens issued by an external identity
-- provider (fzst-claw's Next.js layer).
CREATE TABLE IF NOT EXISTS jwt_verifier_config (
  id                UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
  enabled           BOOLEAN NOT NULL DEFAULT true,
  jwks_url          TEXT NOT NULL,
  issuer            TEXT NOT NULL,
  audience          TEXT NOT NULL,
  cache_ttl_seconds INT NOT NULL DEFAULT 3600,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Idempotent seed: default to localhost Next.js for dev.
INSERT INTO jwt_verifier_config (jwks_url, issuer, audience)
SELECT 'http://localhost:3000/.well-known/jwks.json', 'fzst-claw', 'goclaw'
WHERE NOT EXISTS (SELECT 1 FROM jwt_verifier_config);
