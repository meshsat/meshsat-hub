// Package auth — jwks.go provides OIDC Discovery and JWKS key fetching with caching.
package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// JWKSProvider fetches and caches JSON Web Key Sets from an OIDC issuer.
type JWKSProvider struct {
	issuerURL  string
	httpClient *http.Client

	mu       sync.RWMutex
	keys     map[string]*rsa.PublicKey // kid → public key
	fetchedAt time.Time
	jwksURI  string // discovered from .well-known/openid-configuration

	// cacheTTL controls how long cached keys are valid before a background refresh.
	cacheTTL time.Duration
}

// oidcDiscovery represents the subset of OpenID Connect Discovery we need.
type oidcDiscovery struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

// jwksResponse represents the JWKS endpoint response.
type jwksResponse struct {
	Keys []jwkKey `json:"keys"`
}

// jwkKey represents a single JWK (only RSA supported for now — covers Keycloak, Auth0, etc.).
type jwkKey struct {
	Kty string `json:"kty"` // Key type: "RSA"
	Use string `json:"use"` // Key use: "sig"
	Kid string `json:"kid"` // Key ID
	Alg string `json:"alg"` // Algorithm: "RS256", "RS384", "RS512"
	N   string `json:"n"`   // RSA modulus (base64url)
	E   string `json:"e"`   // RSA exponent (base64url)
}

// NewJWKSProvider creates a JWKS provider for the given OIDC issuer URL.
// It does NOT fetch keys eagerly — keys are fetched on first use or via RefreshKeys.
func NewJWKSProvider(issuerURL string, httpClient *http.Client) *JWKSProvider {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &JWKSProvider{
		issuerURL:  strings.TrimRight(issuerURL, "/"),
		httpClient: httpClient,
		keys:       make(map[string]*rsa.PublicKey),
		cacheTTL:   1 * time.Hour,
	}
}

// GetKey returns the RSA public key for the given kid.
// If the kid is unknown, it attempts a single refresh from the JWKS endpoint
// (handles key rotation at the IdP).
func (p *JWKSProvider) GetKey(kid string) (*rsa.PublicKey, error) {
	// Fast path: check cache.
	p.mu.RLock()
	key, ok := p.keys[kid]
	p.mu.RUnlock()
	if ok {
		return key, nil
	}

	// Cache miss — refresh keys and retry.
	if err := p.RefreshKeys(context.Background()); err != nil {
		return nil, fmt.Errorf("refresh JWKS: %w", err)
	}

	p.mu.RLock()
	key, ok = p.keys[kid]
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown kid %q after JWKS refresh", kid)
	}
	return key, nil
}

// RefreshKeys fetches the JWKS endpoint and updates the key cache.
// If the JWKS URI hasn't been discovered yet, it performs OIDC Discovery first.
func (p *JWKSProvider) RefreshKeys(ctx context.Context) error {
	p.mu.RLock()
	jwksURI := p.jwksURI
	p.mu.RUnlock()

	if jwksURI == "" {
		discovered, err := p.discover(ctx)
		if err != nil {
			return fmt.Errorf("OIDC discovery: %w", err)
		}
		jwksURI = discovered
		p.mu.Lock()
		p.jwksURI = discovered
		p.mu.Unlock()
	}

	keys, err := p.fetchJWKS(ctx, jwksURI)
	if err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}

	p.mu.Lock()
	p.keys = keys
	p.fetchedAt = time.Now()
	p.mu.Unlock()

	slog.Info("auth: JWKS refreshed", "keys", len(keys), "uri", jwksURI)
	return nil
}

// NeedsRefresh returns true if the cache is older than cacheTTL.
func (p *JWKSProvider) NeedsRefresh() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.fetchedAt.IsZero() || time.Since(p.fetchedAt) > p.cacheTTL
}

// discover fetches the OIDC Discovery document and returns the JWKS URI.
func (p *JWKSProvider) discover(ctx context.Context) (string, error) {
	url := p.issuerURL + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
	if err != nil {
		return "", fmt.Errorf("read discovery: %w", err)
	}

	var doc oidcDiscovery
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("parse discovery: %w", err)
	}

	if doc.JWKSURI == "" {
		return "", fmt.Errorf("discovery document missing jwks_uri")
	}

	slog.Info("auth: OIDC discovery complete", "issuer", doc.Issuer, "jwks_uri", doc.JWKSURI)
	return doc.JWKSURI, nil
}

// fetchJWKS fetches and parses the JWKS endpoint, returning a map of kid → RSA public key.
func (p *JWKSProvider) fetchJWKS(ctx context.Context, jwksURI string) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURI, nil)
	if err != nil {
		return nil, err
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", jwksURI, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", jwksURI, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read JWKS: %w", err)
	}

	var jwks jwksResponse
	if err := json.Unmarshal(body, &jwks); err != nil {
		return nil, fmt.Errorf("parse JWKS: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey)
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" {
			continue // skip non-RSA keys (EC support can be added later)
		}
		if k.Use != "" && k.Use != "sig" {
			continue // skip encryption keys
		}
		pub, err := parseRSAPublicKey(k.N, k.E)
		if err != nil {
			slog.Warn("auth: skipping invalid JWKS key", "kid", k.Kid, "error", err)
			continue
		}
		keys[k.Kid] = pub
	}

	if len(keys) == 0 {
		return nil, fmt.Errorf("no usable RSA signing keys in JWKS")
	}

	return keys, nil
}

// parseRSAPublicKey builds an *rsa.PublicKey from base64url-encoded modulus and exponent.
func parseRSAPublicKey(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, fmt.Errorf("decode modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, fmt.Errorf("decode exponent: %w", err)
	}

	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() {
		return nil, fmt.Errorf("exponent too large")
	}

	return &rsa.PublicKey{
		N: n,
		E: int(e.Int64()),
	}, nil
}
