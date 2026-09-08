// Package auth — jwks.go provides OIDC Discovery and JWKS key fetching with caching.
package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
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

	mu        sync.RWMutex
	keys      map[string]crypto.PublicKey // kid → *rsa.PublicKey or *ecdsa.PublicKey
	fetchedAt time.Time
	jwksURI   string // discovered from .well-known/openid-configuration
	discovery *Discovery

	// cacheTTL controls how long cached keys are valid before a background refresh.
	cacheTTL time.Duration
}

// Discovery is the subset of the OpenID Connect Discovery document the Hub
// uses: key verification (jwks_uri), the authorization-code flow endpoints and
// the issuer string the IdP itself claims (authoritative for the iss check).
type Discovery struct {
	Issuer                string `json:"issuer"`
	JWKSURI               string `json:"jwks_uri"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	EndSessionEndpoint    string `json:"end_session_endpoint"`
}

// jwksResponse represents the JWKS endpoint response.
type jwksResponse struct {
	Keys []jwkKey `json:"keys"`
}

// jwkKey represents a single JWK: RSA (Keycloak, Auth0, authentik default) or
// EC P-256/P-384/P-521 (authentik with an EC signing key, Entra).
type jwkKey struct {
	Kty string `json:"kty"` // Key type: "RSA" or "EC"
	Use string `json:"use"` // Key use: "sig"
	Kid string `json:"kid"` // Key ID
	Alg string `json:"alg"` // Algorithm: "RS256"..., "ES256"...
	N   string `json:"n"`   // RSA modulus (base64url)
	E   string `json:"e"`   // RSA exponent (base64url)
	Crv string `json:"crv"` // EC curve: "P-256", "P-384", "P-521"
	X   string `json:"x"`   // EC x coordinate (base64url)
	Y   string `json:"y"`   // EC y coordinate (base64url)
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
		keys:       make(map[string]crypto.PublicKey),
		cacheTTL:   1 * time.Hour,
	}
}

// IssuerURL returns the configured issuer with any trailing slash removed.
func (p *JWKSProvider) IssuerURL() string {
	return p.issuerURL
}

// ExpectedIssuer returns the issuer string tokens must carry: the value the
// discovery document reported when known (authentik's issuer ends with a
// slash, the configured URL usually does not), else the configured URL.
// Compare with IssuerMatches so either spelling validates.
func (p *JWKSProvider) ExpectedIssuer() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.discovery != nil && p.discovery.Issuer != "" {
		return p.discovery.Issuer
	}
	return p.issuerURL
}

// IssuerMatches compares two issuer strings ignoring a trailing slash.
func IssuerMatches(got, want string) bool {
	return strings.TrimRight(got, "/") == strings.TrimRight(want, "/")
}

// Discover returns the OIDC discovery document, fetching it on first use.
func (p *JWKSProvider) Discover(ctx context.Context) (*Discovery, error) {
	p.mu.RLock()
	d := p.discovery
	p.mu.RUnlock()
	if d != nil {
		return d, nil
	}
	if _, err := p.discover(ctx); err != nil {
		return nil, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.discovery, nil
}

// GetKey returns the public key (RSA or ECDSA) for the given kid.
// If the kid is unknown, it attempts a single refresh from the JWKS endpoint
// (handles key rotation at the IdP).
func (p *JWKSProvider) GetKey(kid string) (crypto.PublicKey, error) {
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

	var doc Discovery
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("parse discovery: %w", err)
	}

	if doc.JWKSURI == "" {
		return "", fmt.Errorf("discovery document missing jwks_uri")
	}
	if doc.Issuer != "" && !IssuerMatches(doc.Issuer, p.issuerURL) {
		slog.Warn("auth: discovery issuer differs from configured issuer; tokens are checked against the discovered value",
			"configured", p.issuerURL, "discovered", doc.Issuer)
	}

	p.mu.Lock()
	p.discovery = &doc
	p.mu.Unlock()

	slog.Info("auth: OIDC discovery complete", "issuer", doc.Issuer, "jwks_uri", doc.JWKSURI)
	return doc.JWKSURI, nil
}

// fetchJWKS fetches and parses the JWKS endpoint, returning a map of kid → public key.
func (p *JWKSProvider) fetchJWKS(ctx context.Context, jwksURI string) (map[string]crypto.PublicKey, error) {
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

	keys := make(map[string]crypto.PublicKey)
	for _, k := range jwks.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue // skip encryption keys
		}
		var (
			pub crypto.PublicKey
			err error
		)
		switch k.Kty {
		case "RSA":
			pub, err = parseRSAPublicKey(k.N, k.E)
		case "EC":
			pub, err = parseECPublicKey(k.Crv, k.X, k.Y)
		default:
			continue // OKP and symmetric keys are not accepted for signatures here
		}
		if err != nil {
			slog.Warn("auth: skipping invalid JWKS key", "kid", k.Kid, "kty", k.Kty, "error", err)
			continue
		}
		keys[k.Kid] = pub
	}

	if len(keys) == 0 {
		return nil, fmt.Errorf("no usable RSA or EC signing keys in JWKS")
	}

	return keys, nil
}

// parseECPublicKey builds an *ecdsa.PublicKey from a JWK curve name and
// base64url-encoded coordinates.
func parseECPublicKey(crv, xB64, yB64 string) (*ecdsa.PublicKey, error) {
	var curve elliptic.Curve
	switch crv {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	case "P-521":
		curve = elliptic.P521()
	default:
		return nil, fmt.Errorf("unsupported curve %q", crv)
	}
	xBytes, err := base64.RawURLEncoding.DecodeString(xB64)
	if err != nil {
		return nil, fmt.Errorf("decode x: %w", err)
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(yB64)
	if err != nil {
		return nil, fmt.Errorf("decode y: %w", err)
	}
	x := new(big.Int).SetBytes(xBytes)
	y := new(big.Int).SetBytes(yBytes)
	if !curve.IsOnCurve(x, y) {
		return nil, fmt.Errorf("point is not on curve %s", crv)
	}
	return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
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
