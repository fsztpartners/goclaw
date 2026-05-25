// Package auth provides JWT verification for the gateway HTTP layer.
//
// The verifier accepts RS256 tokens issued by an external identity provider
// (fzst-claw's Next.js layer). The provider exposes a JWKS endpoint; this
// verifier caches the keyset in memory with a TTL, and refetches on a kid
// miss to support seamless key rotation.
package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Config describes how to verify tokens.
type Config struct {
	JWKSURL  string
	Issuer   string
	Audience string
	CacheTTL time.Duration
}

// Claims is the subset of access-token claims the gateway consumes.
type Claims struct {
	Subject  string    // user_id (UUID as text)
	Email    string    // user email
	OrgID    uuid.UUID // tenant_id
	OrgSlug  string    // tenant slug, opaque
	Role     string    // member role: owner|admin|member|viewer
	ExpiresAt time.Time
}

// Verifier verifies RS256 access tokens against a remote JWKS.
type Verifier struct {
	cfg    Config
	client *http.Client

	mu       sync.RWMutex
	keys     map[string]*rsa.PublicKey
	fetchedAt time.Time
}

// NewVerifier constructs a verifier. The first call to Verify triggers the
// initial JWKS fetch.
func NewVerifier(cfg Config) *Verifier {
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = time.Hour
	}
	return &Verifier{
		cfg:    cfg,
		client: &http.Client{Timeout: 5 * time.Second},
		keys:   make(map[string]*rsa.PublicKey),
	}
}

// jwk is the minimal subset of a JWK we parse.
type jwk struct {
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwks struct {
	Keys []jwk `json:"keys"`
}

func (v *Verifier) fetchKeys(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.JWKSURL, nil)
	if err != nil {
		return fmt.Errorf("jwks: new request: %w", err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("jwks: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks: status %d", resp.StatusCode)
	}
	var set jwks
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return fmt.Errorf("jwks: decode: %w", err)
	}
	next := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		pk, err := parseRSAJWK(k)
		if err != nil {
			continue
		}
		next[k.Kid] = pk
	}
	v.mu.Lock()
	v.keys = next
	v.fetchedAt = time.Now()
	v.mu.Unlock()
	return nil
}

func parseRSAJWK(k jwk) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eb, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}
	// e is typically 3 bytes (AQAB → 65537). Pad to 4 for binary.BigEndian.
	if len(eb) < 4 {
		buf := make([]byte, 4)
		copy(buf[4-len(eb):], eb)
		eb = buf
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nb),
		E: int(binary.BigEndian.Uint32(eb)),
	}, nil
}

func (v *Verifier) keyForKid(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	pk, ok := v.keys[kid]
	stale := time.Since(v.fetchedAt) > v.cfg.CacheTTL
	v.mu.RUnlock()
	if ok && !stale {
		return pk, nil
	}
	if err := v.fetchKeys(ctx); err != nil {
		// On refresh failure, fall back to whatever is cached.
		if ok {
			return pk, nil
		}
		return nil, err
	}
	v.mu.RLock()
	pk, ok = v.keys[kid]
	v.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown kid %q", kid)
	}
	return pk, nil
}

// Verify parses and validates a token. Returns Claims on success.
func (v *Verifier) Verify(ctx context.Context, tokenStr string) (*Claims, error) {
	parsed, err := jwt.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != jwt.SigningMethodRS256.Alg() {
			return nil, fmt.Errorf("unexpected alg %q", t.Method.Alg())
		}
		kidRaw, ok := t.Header["kid"].(string)
		if !ok || kidRaw == "" {
			return nil, errors.New("token missing kid")
		}
		return v.keyForKid(ctx, kidRaw)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(v.cfg.Issuer), jwt.WithAudience(v.cfg.Audience))
	if err != nil {
		return nil, err
	}
	if !parsed.Valid {
		return nil, errors.New("token invalid")
	}
	mc, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("unexpected claims shape")
	}
	c := &Claims{}
	if s, ok := mc["sub"].(string); ok {
		c.Subject = s
	}
	if s, ok := mc["email"].(string); ok {
		c.Email = s
	}
	if s, ok := mc["org_id"].(string); ok {
		if id, err := uuid.Parse(s); err == nil {
			c.OrgID = id
		}
	}
	if s, ok := mc["org_slug"].(string); ok {
		c.OrgSlug = s
	}
	if s, ok := mc["role"].(string); ok {
		c.Role = s
	}
	if exp, err := mc.GetExpirationTime(); err == nil && exp != nil {
		c.ExpiresAt = exp.Time
	}
	if c.Subject == "" || c.OrgID == uuid.Nil {
		return nil, errors.New("missing required claims (sub, org_id)")
	}
	return c, nil
}

// LooksLikeJWT returns true if the token string has the three-segment shape
// of a JWT. Used by callers to decide whether to dispatch to the JWT path.
func LooksLikeJWT(token string) bool {
	if len(token) < 20 {
		return false
	}
	dots := 0
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			dots++
			if dots > 2 {
				return false
			}
		}
	}
	return dots == 2
}
