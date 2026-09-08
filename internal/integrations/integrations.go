// Package integrations holds per-tenant provider accounts (MESHSAT-977): the
// Cloudloop, Twilio, Rock7, RockBLOCK and Globalstar credentials a tenant
// configures for itself. Rows live in the tenant-scoped, encrypted
// `credentials` table (target_scope "hub", cred_type "provider_account"); the
// process-wide values from the environment are the platform account and serve
// the default tenant only.
package integrations

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/crypto"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// Provider identifiers (the credentials.provider column).
const (
	ProviderCloudloop  = "cloudloop"
	ProviderTwilio     = "twilio"
	ProviderRock7      = "rock7"
	ProviderRockBLOCK  = "rockblock"
	ProviderGlobalstar = "globalstar"
	ProviderEmail      = "email"

	// CredType marks a provider-account row in the credentials table.
	CredType = "provider_account"
	// Scope is the target_scope of a provider-account row.
	Scope = "hub"
)

// Field describes one configurable value of a provider account.
type Field struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Secret   bool   `json:"secret"`             // write-only, masked on read
	Required bool   `json:"required"`           // must be present to count as configured
	Generate bool   `json:"generate,omitempty"` // filled with a random token when left empty
	Default  string `json:"default,omitempty"`
	Hint     string `json:"hint,omitempty"`
}

// Spec describes a provider.
type Spec struct {
	Provider    string  `json:"provider"`
	Label       string  `json:"label"`
	Description string  `json:"description"`
	Webhook     string  `json:"webhook,omitempty"` // inbound path the tenant configures at the provider
	Fields      []Field `json:"fields"`
}

// Specs lists the supported providers in display order.
var Specs = []Spec{
	{Provider: ProviderCloudloop, Label: "Cloudloop (Iridium SBD/IMT)", Description: "Ground Control Cloudloop account for RockBLOCK 9603/9704 devices: MT sends, credit balance, thing lookup and the MO webhook.",
		Webhook: "/api/webhook/cloudloop",
		Fields: []Field{
			{Key: "api_url", Label: "API URL", Default: "https://api.cloudloop.com"},
			{Key: "api_key", Label: "API key", Secret: true, Required: true},
			{Key: "account_id", Label: "Account ID", Hint: "Used for the Cloudloop MQTT topic; optional."},
			{Key: "webhook_token", Label: "Webhook token", Secret: true, Generate: true, Hint: "The last segment of this tenant's webhook URL. Generated when left empty."},
		}},
	{Provider: ProviderTwilio, Label: "Twilio (SMS)", Description: "Outbound SMS, escalation SMS and the inbound SMS webhook.",
		Webhook: "/api/webhook/sms",
		Fields: []Field{
			{Key: "account_sid", Label: "Account SID", Required: true},
			{Key: "auth_token", Label: "Auth token", Secret: true, Required: true, Hint: "Also validates X-Twilio-Signature on the inbound webhook."},
			{Key: "from_number", Label: "From number", Required: true, Hint: "E.164, e.g. +3197010000000"},
			{Key: "webhook_token", Label: "Webhook token", Secret: true, Generate: true, Hint: "The last segment of this tenant's inbound SMS webhook URL. Generated when left empty."},
			{Key: "webhook_secret", Label: "Webhook signing secret", Secret: true, Hint: "Optional: HMAC-SHA256 of From+Body presented as X-Signature by a custom relay."},
		}},
	{Provider: ProviderRock7, Label: "Rock7 (RockBLOCK MT API)", Description: "Rock7 Core account for direct RockBLOCK MT sends.",
		Fields: []Field{
			{Key: "username", Label: "Username", Required: true},
			{Key: "password", Label: "Password", Secret: true, Required: true},
		}},
	{Provider: ProviderRockBLOCK, Label: "RockBLOCK webhook", Description: "Shared secret Rock7 signs MO webhook deliveries with.",
		Webhook: "/api/webhook/rockblock",
		Fields: []Field{
			{Key: "webhook_secret", Label: "Webhook secret", Secret: true, Required: true, Generate: true},
		}},
	{Provider: ProviderGlobalstar, Label: "Globalstar", Description: "Globalstar API account and the secret its webhook signs with.",
		Webhook: "/api/webhook/globalstar",
		Fields: []Field{
			{Key: "api_url", Label: "API URL"},
			{Key: "api_key", Label: "API key", Secret: true, Required: true},
			{Key: "webhook_secret", Label: "Webhook secret", Secret: true, Required: true, Generate: true},
		}},
	{Provider: ProviderEmail, Label: "Email gateway (inbound)", Description: "The secret the mail relay presents when delivering inbound mail for this tenant.",
		Webhook: "/api/webhook/email",
		Fields: []Field{
			{Key: "webhook_secret", Label: "Webhook secret", Secret: true, Required: true, Generate: true},
		}},
}

// SpecFor returns the spec of a provider.
func SpecFor(provider string) (Spec, bool) {
	for _, s := range Specs {
		if s.Provider == provider {
			return s, true
		}
	}
	return Spec{}, false
}

// Account is a resolved provider account.
type Account struct {
	Provider  string
	TenantID  string
	Fields    map[string]string
	Platform  bool // came from the environment, not from the tenant's row
	UpdatedAt time.Time
}

// Get returns a field value ("" when absent).
func (a *Account) Get(key string) string {
	if a == nil {
		return ""
	}
	return a.Fields[key]
}

// ErrUnknownProvider is returned for a provider outside Specs.
var ErrUnknownProvider = errors.New("unknown provider")

// ErrNotConfigured is returned by Require when the tenant has no account.
var ErrNotConfigured = errors.New("provider account not configured for this tenant")

// Service reads and writes provider accounts with a short cache.
type Service struct {
	store     store.Store
	key       []byte
	ttl       time.Duration
	defaultID string

	mu       sync.RWMutex
	platform map[string]*Account           // provider -> env account (default tenant fallback)
	cache    map[string]map[string]*cached // tenant -> provider -> entry (nil account = not configured)
	tokens   map[string]tokenHit           // sha256(provider|token) -> tenant
}

type cached struct {
	acct *Account
	at   time.Time
}

type tokenHit struct {
	tenant string
	at     time.Time
}

// New creates a service over the store with the credentials master key.
func New(s store.Store, masterKey []byte) *Service {
	return &Service{store: s, key: masterKey, ttl: time.Minute, defaultID: store.DefaultTenantID,
		platform: map[string]*Account{}, cache: map[string]map[string]*cached{}, tokens: map[string]tokenHit{}}
}

// SetPlatform registers the environment-configured account of a provider. Only
// fields with a value are kept; an account without its required fields is
// treated as absent.
func (s *Service) SetPlatform(provider string, fields map[string]string) {
	spec, ok := SpecFor(provider)
	if !ok {
		return
	}
	clean := map[string]string{}
	for k, v := range fields {
		if v = strings.TrimSpace(v); v != "" {
			clean[k] = v
		}
	}
	for _, f := range spec.Fields {
		if f.Required && clean[f.Key] == "" {
			s.mu.Lock()
			delete(s.platform, provider)
			s.mu.Unlock()
			return
		}
	}
	s.mu.Lock()
	s.platform[provider] = &Account{Provider: provider, TenantID: s.defaultID, Fields: clean, Platform: true}
	s.mu.Unlock()
}

// Platform returns the environment account of a provider (nil when unset).
func (s *Service) Platform(provider string) *Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.platform[provider]
}

// ForTenant returns the tenant's account for a provider, the platform account
// for the default tenant when it has none, or nil when neither exists.
func (s *Service) ForTenant(ctx context.Context, tenantID, provider string) (*Account, error) {
	if _, ok := SpecFor(provider); !ok {
		return nil, ErrUnknownProvider
	}
	if tenantID == "" {
		tenantID = s.defaultID
	}
	s.mu.RLock()
	if c, ok := s.cache[tenantID][provider]; ok && time.Since(c.at) < s.ttl {
		s.mu.RUnlock()
		return c.acct, nil
	}
	s.mu.RUnlock()

	acct, err := s.load(ctx, tenantID, provider)
	if err != nil {
		return nil, err
	}
	if acct == nil && tenantID == s.defaultID {
		acct = s.Platform(provider)
	}
	s.mu.Lock()
	if s.cache[tenantID] == nil {
		s.cache[tenantID] = map[string]*cached{}
	}
	s.cache[tenantID][provider] = &cached{acct: acct, at: time.Now()}
	s.mu.Unlock()
	return acct, nil
}

// Require is ForTenant that fails when no account exists.
func (s *Service) Require(ctx context.Context, tenantID, provider string) (*Account, error) {
	a, err := s.ForTenant(ctx, tenantID, provider)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, ErrNotConfigured
	}
	return a, nil
}

func (s *Service) load(ctx context.Context, tenantID, provider string) (*Account, error) {
	row, err := s.row(ctx, tenantID, provider)
	if err != nil || row == nil {
		return nil, err
	}
	return s.decode(row)
}

func (s *Service) row(ctx context.Context, tenantID, provider string) (*store.Credential, error) {
	creds, err := s.store.ListCredentials(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	for i := range creds {
		c := &creds[i]
		if c.Provider == provider && c.CredType == CredType && c.TargetScope == Scope && c.Status != "revoked" {
			return c, nil
		}
	}
	return nil, nil
}

func (s *Service) decode(c *store.Credential) (*Account, error) {
	plain, err := crypto.Decrypt(s.key, c.EncryptedData)
	if err != nil {
		return nil, fmt.Errorf("integrations: decrypt %s/%s: %w", c.TenantID, c.Provider, err)
	}
	var fields map[string]string
	if err := json.Unmarshal(plain, &fields); err != nil {
		return nil, fmt.Errorf("integrations: decode %s/%s: %w", c.TenantID, c.Provider, err)
	}
	return &Account{Provider: c.Provider, TenantID: c.TenantID, Fields: fields, UpdatedAt: c.UpdatedAt}, nil
}

// Set stores (creates or updates) a tenant's account. Secret fields left empty
// keep their stored value, so a form can change a URL without re-entering the
// key; generated fields are filled when still empty. Returns the stored
// account (with secrets).
func (s *Service) Set(ctx context.Context, tenantID, provider string, fields map[string]string) (*Account, error) {
	spec, ok := SpecFor(provider)
	if !ok {
		return nil, ErrUnknownProvider
	}
	if tenantID == "" {
		return nil, errors.New("tenant required")
	}
	existing, err := s.row(ctx, tenantID, provider)
	if err != nil {
		return nil, err
	}
	merged := map[string]string{}
	if existing != nil {
		if old, err := s.decode(existing); err == nil {
			for k, v := range old.Fields {
				merged[k] = v
			}
		}
	}
	known := map[string]Field{}
	for _, f := range spec.Fields {
		known[f.Key] = f
	}
	for k, v := range fields {
		f, ok := known[k]
		if !ok {
			return nil, fmt.Errorf("unknown field %q for %s", k, provider)
		}
		v = strings.TrimSpace(v)
		if len(v) > 4096 {
			return nil, fmt.Errorf("field %q too long", k)
		}
		if v == "" {
			if f.Secret {
				continue // keep the stored secret
			}
			delete(merged, k)
			continue
		}
		merged[k] = v
	}
	for _, f := range spec.Fields {
		if merged[f.Key] == "" && f.Generate {
			merged[f.Key] = randomToken()
		}
		if merged[f.Key] == "" && f.Default != "" {
			merged[f.Key] = f.Default
		}
	}
	for _, f := range spec.Fields {
		if f.Required && merged[f.Key] == "" {
			return nil, fmt.Errorf("%s is required", f.Label)
		}
	}
	plain, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	enc, err := crypto.Encrypt(s.key, plain)
	if err != nil {
		return nil, fmt.Errorf("integrations: encrypt: %w", err)
	}
	now := time.Now().UTC()
	if existing != nil {
		existing.EncryptedData = enc
		existing.Status = "active"
		existing.Version++
		existing.UpdatedAt = now
		if err := s.store.UpdateCredential(ctx, tenantID, existing); err != nil {
			return nil, err
		}
	} else {
		c := &store.Credential{ID: "acct_" + provider + "_" + randomID(), TenantID: tenantID, Provider: provider, Name: spec.Label,
			CredType: CredType, EncryptedData: enc, TargetScope: Scope, Status: "active", Version: 1, CreatedAt: now, UpdatedAt: now}
		if err := s.store.CreateCredential(ctx, tenantID, c); err != nil {
			return nil, err
		}
	}
	s.Invalidate(tenantID)
	return &Account{Provider: provider, TenantID: tenantID, Fields: merged, UpdatedAt: now}, nil
}

// Delete removes a tenant's account for a provider.
func (s *Service) Delete(ctx context.Context, tenantID, provider string) error {
	row, err := s.row(ctx, tenantID, provider)
	if err != nil {
		return err
	}
	if row != nil {
		if err := s.store.DeleteCredential(ctx, tenantID, row.ID); err != nil {
			return err
		}
	}
	s.Invalidate(tenantID)
	return nil
}

// TenantsWith lists the tenants that have an account for the provider; the
// default tenant is included when a platform account exists.
func (s *Service) TenantsWith(ctx context.Context, provider string) ([]string, error) {
	rows, err := s.store.ListHubCredentialsByProvider(ctx, provider)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for i := range rows {
		if rows[i].CredType == CredType && rows[i].Status != "revoked" && !seen[rows[i].TenantID] {
			seen[rows[i].TenantID] = true
			out = append(out, rows[i].TenantID)
		}
	}
	if s.Platform(provider) != nil && !seen[s.defaultID] {
		out = append(out, s.defaultID)
	}
	sort.Strings(out)
	return out, nil
}

// Invalidate drops the cache for a tenant (and all token hits).
func (s *Service) Invalidate(tenantID string) {
	s.mu.Lock()
	delete(s.cache, tenantID)
	s.tokens = map[string]tokenHit{}
	s.mu.Unlock()
}

// Masked returns the fields for display: secrets reduced to their last four
// characters, absent values as "".
func Masked(spec Spec, a *Account) map[string]string {
	out := map[string]string{}
	for _, f := range spec.Fields {
		v := a.Get(f.Key)
		if v == "" {
			out[f.Key] = ""
			continue
		}
		if f.Secret {
			if len(v) > 8 {
				out[f.Key] = "••••" + v[len(v)-4:]
			} else {
				out[f.Key] = "••••"
			}
			continue
		}
		out[f.Key] = v
	}
	return out
}

// LookupByToken finds the tenant whose account for the provider carries the
// given token in fieldKey (e.g. Cloudloop "webhook_token"). The platform
// account maps to the default tenant. Returns "" when nothing matches.
func (s *Service) LookupByToken(ctx context.Context, provider, fieldKey, token string) (string, *Account, error) {
	if token == "" {
		return "", nil, nil
	}
	h := tokenHash(provider, fieldKey, token)
	s.mu.RLock()
	hit, ok := s.tokens[h]
	s.mu.RUnlock()
	if ok && time.Since(hit.at) < s.ttl {
		a, err := s.ForTenant(ctx, hit.tenant, provider)
		if err == nil && a != nil && subtle.ConstantTimeCompare([]byte(a.Get(fieldKey)), []byte(token)) == 1 {
			return hit.tenant, a, nil
		}
	}
	rows, err := s.store.ListHubCredentialsByProvider(ctx, provider)
	if err != nil {
		return "", nil, err
	}
	// Deterministic order so two tenants with the same token (should never
	// happen: tokens are generated) resolve the same way every time.
	sort.Slice(rows, func(i, j int) bool { return rows[i].TenantID < rows[j].TenantID })
	for i := range rows {
		if rows[i].CredType != CredType {
			continue
		}
		a, err := s.decode(&rows[i])
		if err != nil {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(a.Get(fieldKey)), []byte(token)) == 1 {
			s.remember(h, a.TenantID)
			return a.TenantID, a, nil
		}
	}
	if p := s.Platform(provider); p != nil && subtle.ConstantTimeCompare([]byte(p.Get(fieldKey)), []byte(token)) == 1 {
		s.remember(h, s.defaultID)
		return s.defaultID, p, nil
	}
	return "", nil, nil
}

func (s *Service) remember(h, tenant string) {
	s.mu.Lock()
	s.tokens[h] = tokenHit{tenant: tenant, at: time.Now()}
	s.mu.Unlock()
}

func tokenHash(provider, key, token string) string {
	sum := sha256.Sum256([]byte(provider + "\x00" + key + "\x00" + token))
	return hex.EncodeToString(sum[:])
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// WebhookSecretField names the field whose value is the last segment of the
// provider's per-tenant webhook URL. Empty when the provider has no inbound
// webhook.
func WebhookSecretField(provider string) string {
	switch provider {
	case ProviderCloudloop, ProviderTwilio:
		return "webhook_token"
	case ProviderRockBLOCK, ProviderGlobalstar, ProviderEmail:
		return "webhook_secret"
	}
	return ""
}
