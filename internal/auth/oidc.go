package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// OIDCClient performs the authorization-code flow (with PKCE) against an
// OpenID Connect provider and verifies the resulting ID token with the
// provider's JWKS. It uses only net/http and the JWT library already in use.
type OIDCClient struct {
	Provider     *JWKSProvider
	ClientID     string
	ClientSecret string
	RedirectURI  string
	Scopes       string
	HTTPClient   *http.Client
}

// OIDCState is the per-login state carried in a signed cookie between the
// redirect to the IdP and the callback.
type OIDCState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"next,omitempty"`
	Expires  int64  `json:"exp"`
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewOIDCState generates state, nonce and a PKCE verifier valid for ttl.
func NewOIDCState(next string, ttl time.Duration) (*OIDCState, error) {
	s, err := randomToken(24)
	if err != nil {
		return nil, err
	}
	n, err := randomToken(24)
	if err != nil {
		return nil, err
	}
	v, err := randomToken(48)
	if err != nil {
		return nil, err
	}
	return &OIDCState{State: s, Nonce: n, Verifier: v, Next: next, Expires: time.Now().Add(ttl).Unix()}, nil
}

// PKCEChallenge derives the S256 code challenge from a verifier.
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// EncodeState signs the state with key (HMAC-SHA256) for storage in a cookie.
func EncodeState(st *OIDCState, key []byte) (string, error) {
	body, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// ErrOIDCState is returned for a missing, tampered or expired state cookie.
var ErrOIDCState = errors.New("oidc: invalid or expired login state")

// DecodeState verifies and decodes a state cookie value.
func DecodeState(value string, key []byte) (*OIDCState, error) {
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		return nil, ErrOIDCState
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0]))
	want := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(got, want) {
		return nil, ErrOIDCState
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrOIDCState
	}
	var st OIDCState
	if err := json.Unmarshal(body, &st); err != nil {
		return nil, ErrOIDCState
	}
	if time.Now().Unix() > st.Expires {
		return nil, ErrOIDCState
	}
	return &st, nil
}

// AuthURL builds the authorization request URL for the given state.
func (c *OIDCClient) AuthURL(ctx context.Context, st *OIDCState) (string, error) {
	disc, err := c.Provider.Discover(ctx)
	if err != nil {
		return "", err
	}
	if disc.AuthorizationEndpoint == "" {
		return "", errors.New("oidc: discovery has no authorization_endpoint")
	}
	scopes := c.Scopes
	if scopes == "" {
		scopes = "openid profile email"
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", c.ClientID)
	q.Set("redirect_uri", c.RedirectURI)
	q.Set("scope", scopes)
	q.Set("state", st.State)
	q.Set("nonce", st.Nonce)
	q.Set("code_challenge", PKCEChallenge(st.Verifier))
	q.Set("code_challenge_method", "S256")
	sep := "?"
	if strings.Contains(disc.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return disc.AuthorizationEndpoint + sep + q.Encode(), nil
}

// tokenResponse is the subset of the token endpoint response we use.
type tokenResponse struct {
	IDToken     string `json:"id_token"`
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

// Exchange redeems the authorization code (client_secret_basic + PKCE) and
// returns the raw ID token.
func (c *OIDCClient) Exchange(ctx context.Context, code, verifier string) (string, error) {
	disc, err := c.Provider.Discover(ctx)
	if err != nil {
		return "", err
	}
	if disc.TokenEndpoint == "" {
		return "", errors.New("oidc: discovery has no token_endpoint")
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", c.RedirectURI)
	form.Set("code_verifier", verifier)
	form.Set("client_id", c.ClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, disc.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(url.QueryEscape(c.ClientID), url.QueryEscape(c.ClientSecret))
	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("oidc: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("oidc: read token response: %w", err)
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("oidc: parse token response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || tr.Error != "" {
		return "", fmt.Errorf("oidc: token endpoint status %d: %s %s", resp.StatusCode, tr.Error, tr.ErrorDesc)
	}
	if tr.IDToken == "" {
		return "", errors.New("oidc: token response has no id_token")
	}
	return tr.IDToken, nil
}

// VerifyIDToken checks signature (JWKS), issuer, audience (client id),
// expiry and nonce, and returns the claims.
func (c *OIDCClient) VerifyIDToken(raw, nonce string) (jwt.MapClaims, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512"}),
		jwt.WithExpirationRequired(),
		jwt.WithAudience(c.ClientID),
	)
	token, err := parser.Parse(raw, func(t *jwt.Token) (interface{}, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("missing kid")
		}
		return c.Provider.GetKey(kid)
	})
	if err != nil {
		return nil, fmt.Errorf("oidc: id_token: %w", err)
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("oidc: id_token claims")
	}
	iss, _ := claims["iss"].(string)
	if !IssuerMatches(iss, c.Provider.ExpectedIssuer()) {
		return nil, fmt.Errorf("oidc: issuer %q does not match %q", iss, c.Provider.ExpectedIssuer())
	}
	if got, _ := claims["nonce"].(string); nonce != "" && got != nonce {
		return nil, errors.New("oidc: nonce mismatch")
	}
	return claims, nil
}

// StringSliceClaim reads a claim that may be a JSON array of strings or a
// single string.
func StringSliceClaim(claims jwt.MapClaims, name string) []string {
	switch v := claims[name].(type) {
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	}
	return nil
}
