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
	"github.com/meshsat/meshsat-hub/internal/netguard"
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
	// ProviderTAK is a tenant's OWN TAK/CoT server (MESHSAT-1065), as opposed to
	// the hosted per-tenant instance the operator runs for them.
	ProviderTAK = "tak"

	// ProviderAPRSIS is a tenant's OWN amateur-radio licence (MESHSAT-1121).
	// Unlike every other provider here, the credential is not an account with a
	// company: a callsign and its APRS-IS passcode are issued to a NAMED LICENSED
	// OPERATOR, and transmitting under somebody else's is not a misconfiguration,
	// it is using their licence. So there is deliberately no inheritance -- a
	// tenant with no APRS-IS account of its own injects nothing, rather than
	// falling back to the operator's callsign.
	ProviderAPRSIS = "aprsis"

	// ProviderApprise and ProviderNtfy are where a tenant's alerts are DELIVERED
	// (MESHSAT-1121). The targets were always per tenant and per device
	// (store.NotificationPref); only the backend that delivers to them was one
	// global value, and it was unset in production -- so every notification URL a
	// customer saved was accepted and delivered nowhere.
	ProviderApprise = "apprise"
	ProviderNtfy    = "ntfy"

	// ProviderWireGuard and ProviderHawkbit are infrastructure a tenant runs for
	// its OWN field devices -- a VPN the kit dials into, a firmware server it
	// updates from (MESHSAT-1121). Neither was ever deployed by the operator, so
	// both were a nav entry in front of nothing; bring-your-own is what they
	// should have been from the start.
	ProviderWireGuard = "wireguard"
	ProviderHawkbit   = "hawkbit"

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
	// URL marks a field the Hub will make outbound requests to. Those are
	// validated against internal/netguard on save, because a tenant choosing a
	// URL the Hub then fetches is a request-forgery primitive: the Hub sits in a
	// cluster with a database, a broker and a metadata service all reachable by
	// name (MESHSAT-1121, found by gosec as a G704 taint).
	URL bool `json:"url,omitempty"`
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
			{Key: "api_url", Label: "API URL", Default: "https://api.cloudloop.com", URL: true},
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
			{Key: "api_url", Label: "API URL", URL: true},
			{Key: "api_key", Label: "API key", Secret: true, Required: true},
			{Key: "webhook_secret", Label: "Webhook secret", Secret: true, Required: true, Generate: true},
		}},
	// Both halves of the PGP email gateway. The webhook secret (inbound) has been
	// here since MESHSAT-977; the SENDING half was added by MESHSAT-1121, where it
	// had been six global environment variables, so every tenant's alerts would
	// have left from one address signed with one key.
	//
	// None of the sending fields is Required, deliberately. A tenant that already
	// has a row with only webhook_secret must keep being able to save it: Set
	// enforces Required across the whole account, so marking smtp_host required
	// would reject every existing inbound-only account on its next edit.
	{Provider: ProviderEmail, Label: "Email gateway (PGP)", Description: "Send and receive PGP email for this tenant. The webhook secret authenticates inbound mail; the SMTP details below are the address your alerts are sent from. Leave the SMTP fields empty to receive only.",
		Webhook: "/api/webhook/email",
		Fields: []Field{
			{Key: "webhook_secret", Label: "Webhook secret", Secret: true, Required: true, Generate: true, Hint: "authenticates inbound mail delivered for this tenant."},
			{Key: "smtp_host", Label: "SMTP host", Hint: "host:port of your outgoing mail server; leave empty to receive only."},
			{Key: "from", Label: "From address", Hint: "the address your alerts are sent from."},
			{Key: "username", Label: "SMTP username", Hint: "leave empty if your relay authorises by address."},
			{Key: "password", Label: "SMTP password", Secret: true},
			{Key: "pgp_key", Label: "PGP private key (armored)", Secret: true, Hint: "your own key, used to sign outgoing mail and decrypt inbound. Leave empty and one is generated per run, which is fine for encrypting TO contacts but cannot decrypt anything sent to a previous run."},
		}},
	// Bring your own TAK server (MESHSAT-1065). A tenant that already runs TAK
	// Server, FreeTAKServer, OpenTAKServer or taky points the Hub at it and their
	// devices' positions, SOS and telemetry are forwarded there as CoT.
	//
	// Outbound only: their phones connect to their own server directly, so the
	// Hub's TAK front is not involved and this works whether or not it is enabled.
	//
	// The client key is a secret like any other and is stored encrypted; the
	// certificate and CA are not secret but are kept in the same row because they
	// are useless apart. server_name is separate from host on purpose: an address
	// and the name on a certificate are different things, and leaving it empty is
	// what asks for the chain to be verified while skipping the name check -- which
	// is the only workable setting for a server whose certificate names something
	// its address does not.
	{Provider: ProviderTAK, Label: "Your own TAK server", Description: "Forward this tenant's positions, SOS and telemetry as Cursor-on-Target to a TAK server you run. Mutual TLS: the Hub presents the client certificate below and verifies your server against the CA you give it.",
		Fields: []Field{
			{Key: "host", Label: "Host", Required: true, Hint: "hostname or address of the CoT listener, without a port"},
			{Key: "port", Label: "Port", Required: true, Default: "8089", Hint: "the TLS CoT port, 8089 on a stock TAK server"},
			{Key: "server_name", Label: "Certificate name", Hint: "leave empty unless your server's certificate names something other than the host above; empty verifies the chain and skips the name check"},
			{Key: "ca_pem", Label: "Server CA (PEM)", Required: true, Hint: "the authority that signed your server's certificate"},
			{Key: "client_cert_pem", Label: "Client certificate (PEM)", Required: true, Hint: "the certificate your server accepts from this Hub"},
			{Key: "client_key_pem", Label: "Client key (PEM)", Secret: true, Required: true, Hint: "the private key for that certificate; stored encrypted and never shown again"},
		}},
	// APRS-IS (MESHSAT-1121). The callsign is the tenant's own amateur licence,
	// so `callsign` and `passcode` are both Required: a half-filled account must
	// not resolve, or the Hub would hold a connection it cannot authenticate and
	// the operator would see a login failure they did not cause.
	//
	// `enabled` is a field rather than "the row exists" on purpose. Deleting the
	// account is the way to remove a licence from the Hub; pausing transmission
	// for a weekend should not make somebody re-enter a passcode they may have to
	// go and look up again.
	{Provider: ProviderAPRSIS, Label: "APRS-IS (your callsign)", Description: "Inject your devices' satellite positions into the APRS-IS network under YOUR amateur radio callsign. Requires a licence: the passcode is derived from the callsign and is not a password you choose. Nothing is transmitted until you fill this in -- the Hub never puts your traffic on air under anyone else's callsign.",
		Fields: []Field{
			{Key: "callsign", Label: "Callsign", Required: true, Hint: "your licensed callsign, e.g. PD1ABC. Without the SSID."},
			{Key: "ssid", Label: "SSID", Default: "10", Hint: "0-15, appended as -N. 10 is the usual choice for an internet gateway."},
			{Key: "passcode", Label: "APRS-IS passcode", Secret: true, Required: true, Hint: "the numeric passcode for that callsign. Not a password: it is derived from the callsign itself."},
			{Key: "server", Label: "Server", Default: "rotate.aprs2.net:14580", Hint: "host:port of an APRS-IS core server; the default rotates across the pool."},
			{Key: "enabled", Label: "Transmit", Default: "true", Hint: "set to false to stop transmitting without deleting your callsign and passcode."},
		}},
	// Apprise and ntfy are DELIVERY backends, not accounts. What a tenant enters
	// here is the server their alerts go through; who the alerts reach is already
	// per tenant and per device on the Notifications page.
	{Provider: ProviderApprise, Label: "Apprise (notification relay)", Description: "The Apprise server your alerts are delivered through. The notification URLs you set per device on the Notifications page are handed to this server. Without one, those URLs are stored and never delivered to.",
		Fields: []Field{
			{Key: "url", Label: "Apprise API URL", Required: true, URL: true, Hint: "base URL of an Apprise API server, e.g. https://apprise.example.org"},
		}},
	{Provider: ProviderWireGuard, Label: "WireGuard (wg-easy)", Description: "A wg-easy server your field devices dial into. The Hub creates a peer when you register a device and removes it when you delete one. Peers are created on YOUR server; nothing is shared with another tenant.",
		Fields: []Field{
			{Key: "url", Label: "wg-easy URL", Required: true, URL: true, Hint: "e.g. https://vpn.example.org"},
			{Key: "password", Label: "wg-easy password", Secret: true, Required: true},
		}},
	{Provider: ProviderHawkbit, Label: "hawkBit (OTA firmware)", Description: "An Eclipse hawkBit server for firmware rollouts to your own fleet. hawkBit is multi-tenant itself, so the account below decides which of its tenants the Hub acts in.",
		Fields: []Field{
			{Key: "url", Label: "hawkBit URL", Required: true, URL: true, Hint: "e.g. https://hawkbit.example.org"},
			{Key: "username", Label: "Username", Required: true},
			{Key: "password", Label: "Password", Secret: true, Required: true},
		}},
	{Provider: ProviderNtfy, Label: "ntfy (push notifications)", Description: "The ntfy server your push notifications are published to. Use your own, or the public ntfy.sh. Alert targets that name an ntfy topic are published here.",
		Fields: []Field{
			{Key: "url", Label: "ntfy server URL", Required: true, URL: true, Default: "https://ntfy.sh", Hint: "e.g. https://ntfy.sh or your own instance"},
			{Key: "token", Label: "Access token", Secret: true, Hint: "only needed for a protected topic; leave empty for a public one."},
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
	// noURLCheck disables the save-time SSRF check on URL fields.
	//
	// TEST SEAM, and the only callers are tests in packages that point a provider
	// at an httptest server, which binds to 127.0.0.1 -- exactly what the check
	// refuses and exactly what it should refuse. Those tests are about which
	// account a tenant resolves to; the check itself is covered by
	// internal/integrations/ssrf_test.go and internal/netguard.
	//
	// Unexported with no production setter, so configuration cannot reach it.
	noURLCheck bool

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
	// Every URL the Hub will make outbound requests to, checked BEFORE it is
	// stored. A tenant choosing a URL the Hub then fetches is a request-forgery
	// primitive: the Hub sits in a cluster with a database, a broker, an object
	// store and a cloud metadata service all reachable by name or by RFC1918
	// address (MESHSAT-1121).
	//
	// This is the chokepoint for ALL providers rather than a check in each pool,
	// so a provider added later inherits it by marking its field URL: true.
	// Rejecting here also gives the customer an error while they are looking at
	// the form, rather than a send that quietly fails later.
	//
	// SetPlatform deliberately does NOT come through here: the operator's own
	// http://wg-easy:51821 is exactly what this refuses, and it is correct for
	// the platform to use it. The difference is who chose the value.
	for _, f := range spec.Fields {
		if !f.URL || merged[f.Key] == "" || s.noURLCheck {
			continue
		}
		if err := netguard.ValidatePublicURL(merged[f.Key]); err != nil {
			return nil, fmt.Errorf("%s: %w", f.Label, err)
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
