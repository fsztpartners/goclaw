package cmd

import (
	"context"
	"log/slog"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/auth"
	httpapi "github.com/nextlevelbuilder/goclaw/internal/http"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// initJWTVerifierFromDB reads the single-row jwt_verifier_config table and
// installs an RS256 verifier on the HTTP auth chain. If the row is missing
// or disabled, JWT auth simply isn't available (no error — the gateway falls
// back to its existing auth methods).
func initJWTVerifierFromDB(s *store.Stores) {
	if s == nil || s.DB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	row := s.DB.QueryRowContext(ctx, `
		SELECT enabled, jwks_url, issuer, audience, cache_ttl_seconds
		FROM jwt_verifier_config
		ORDER BY created_at DESC
		LIMIT 1
	`)
	var (
		enabled  bool
		jwksURL  string
		issuer   string
		audience string
		ttlSec   int
	)
	if err := row.Scan(&enabled, &jwksURL, &issuer, &audience, &ttlSec); err != nil {
		slog.Info("jwt.verifier_config_absent", "error", err)
		return
	}
	if !enabled || jwksURL == "" {
		slog.Info("jwt.verifier_disabled")
		return
	}
	v := auth.NewVerifier(auth.Config{
		JWKSURL:  jwksURL,
		Issuer:   issuer,
		Audience: audience,
		CacheTTL: time.Duration(ttlSec) * time.Second,
	})
	httpapi.InitJWTVerifier(v)
	slog.Info("jwt.verifier_enabled", "jwks_url", jwksURL, "issuer", issuer, "audience", audience)
}
