package lxmfhub

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/lxmf"
	"github.com/meshsat/meshsat-hub/internal/rnswire"
)

// ErrNoIdentity: the tenant has not created an LXMF address.
var ErrNoIdentity = errors.New("lxmf: this tenant has no LXMF identity")

// Limits on what an owner may set.
const (
	MaxDisplayName = 64
	MaxStampCost   = 32
)

// Identity is a tenant's LXMF address as the API shows it.
type Identity struct {
	TenantID string `json:"-"`
	// Address is the lxmf.delivery destination hash, 32 hex characters: what
	// another client enters to message this tenant.
	Address     string `json:"address"`
	DisplayName string `json:"display_name"`
	StampCost   int    `json:"stamp_cost"`
	Enabled     bool   `json:"enabled"`
	// PropagationAddress is the lxmf.propagation hash on the same identity:
	// what a client enters as its propagation node. PropagationStampCost is
	// the proof of work a client attaches to a message it hands the node.
	PropagationAddress   string `json:"propagation_address"`
	PropagationEnabled   bool   `json:"propagation_enabled"`
	PropagationStampCost int    `json:"propagation_stamp_cost"`

	ident *rnswire.Identity
}

// IdentityUpdate changes what an owner may change; nil leaves a field alone.
type IdentityUpdate struct {
	DisplayName          *string
	StampCost            *int
	Enabled              *bool
	PropagationEnabled   *bool
	PropagationStampCost *int
}

// PrivateKeyHex renders an identity the way Reticulum's own identity files
// store it: 32 bytes X25519 private key then the 32-byte Ed25519 seed, so
// RNS.Identity.from_bytes reads it and derives the same hashes.
func PrivateKeyHex(id *rnswire.Identity) string {
	sig := id.SigningPrivateBytes()
	return hex.EncodeToString(append(append([]byte(nil), id.EncryptionPrivateBytes()...), sig[:ed25519.SeedSize]...))
}

// identityFromHex is the inverse of PrivateKeyHex.
func identityFromHex(h string) (*rnswire.Identity, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(h))
	if err != nil || len(raw) != 64 {
		return nil, errors.New("lxmf: identity key must be 64 bytes hex")
	}
	return rnswire.LoadIdentity(raw[:32], ed25519.NewKeyFromSeed(raw[32:]))
}

func identityFromAccount(tenantID, tenantName string, acct *integrations.Account) (*Identity, error) {
	if acct == nil || acct.Get("private_key") == "" {
		return nil, ErrNoIdentity
	}
	ident, err := identityFromHex(acct.Get("private_key"))
	if err != nil {
		return nil, err
	}
	id := &Identity{TenantID: tenantID, ident: ident, DisplayName: acct.Get("display_name"), Enabled: true}
	h := ident.DestHash(lxmf.DeliveryName)
	id.Address = hex.EncodeToString(h[:])
	if id.DisplayName == "" {
		id.DisplayName = defaultDisplayName(tenantName)
	}
	if v := acct.Get("stamp_cost"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= MaxStampCost {
			id.StampCost = n
		}
	}
	if v := acct.Get("enabled"); v != "" {
		id.Enabled = v == "true" || v == "1"
	}
	ph := ident.DestHash(lxmf.PropagationName)
	id.PropagationAddress = hex.EncodeToString(ph[:])
	id.PropagationEnabled = true
	if v := acct.Get("propagation_enabled"); v != "" {
		id.PropagationEnabled = v == "true" || v == "1"
	}
	id.PropagationStampCost = lxmf.DefaultPropagationCost
	if v := acct.Get("propagation_stamp_cost"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= MaxStampCost {
			id.PropagationStampCost = n
		}
	}
	return id, nil
}

func defaultDisplayName(tenantName string) string {
	n := strings.TrimSpace(tenantName)
	if n == "" {
		return "MeshSat Hub"
	}
	if utf8.RuneCountInString(n) > MaxDisplayName {
		n = string([]rune(n)[:MaxDisplayName])
	}
	return n
}

// LoadIdentity reads the tenant's identity without creating one.
func (s *Service) LoadIdentity(ctx context.Context, tenantID string) (*Identity, error) {
	acct, err := s.accounts.ForTenant(ctx, tenantID, integrations.ProviderLXMF)
	if err != nil {
		return nil, err
	}
	return identityFromAccount(tenantID, s.tenantName(ctx, tenantID), acct)
}

func (s *Service) tenantName(ctx context.Context, tenantID string) string {
	t, err := s.store.GetTenant(ctx, tenantID)
	if err != nil || t == nil {
		return ""
	}
	return t.Name
}

// EnsureIdentity returns the tenant's identity, creating it on first use.
// One replica creates (a claim per tenant and minute); the other waits for
// what was stored; neither answers with a key it generated but did not keep.
// Saving the row notifies every replica, and the leader registers the new
// address on the node.
func (s *Service) EnsureIdentity(ctx context.Context, tenantID, actor string) (*Identity, error) {
	id, err := s.LoadIdentity(ctx, tenantID)
	if !errors.Is(err, ErrNoIdentity) {
		return id, err
	}
	won, err := s.store.ClaimOnce(ctx, fmt.Sprintf("lxmf-identity:%s:%d", tenantID, s.now().Unix()/60))
	if err != nil {
		return nil, err
	}
	if won {
		fresh, err := rnswire.GenerateIdentity()
		if err != nil {
			return nil, err
		}
		if _, err := s.accounts.Set(ctx, tenantID, integrations.ProviderLXMF, map[string]string{
			"private_key": PrivateKeyHex(fresh), "display_name": defaultDisplayName(s.tenantName(ctx, tenantID)),
			"stamp_cost": "0", "enabled": "true",
		}); err != nil {
			return nil, err
		}
		h := fresh.DestHash(lxmf.DeliveryName)
		s.auditLog(ctx, tenantID, "lxmf_identity_created", actor, "address "+hex.EncodeToString(h[:]))
	}
	for i := 0; i < 20; i++ {
		id, err = s.LoadIdentity(ctx, tenantID)
		if !errors.Is(err, ErrNoIdentity) {
			return id, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return nil, errors.New("lxmf: the identity was not stored in time")
}

// UpdateIdentity changes the display name, stamp cost or enabled flag.
func (s *Service) UpdateIdentity(ctx context.Context, tenantID, actor string, upd IdentityUpdate) (*Identity, error) {
	if _, err := s.LoadIdentity(ctx, tenantID); err != nil {
		return nil, err
	}
	fields := map[string]string{}
	var changes []string
	if upd.DisplayName != nil {
		n := strings.TrimSpace(*upd.DisplayName)
		if n == "" || utf8.RuneCountInString(n) > MaxDisplayName || strings.ContainsAny(n, "\n\r\x00") {
			return nil, fmt.Errorf("lxmf: display name must be 1 to %d characters on one line", MaxDisplayName)
		}
		fields["display_name"] = n
		changes = append(changes, "display name "+strconv.Quote(n))
	}
	if upd.StampCost != nil {
		if *upd.StampCost < 0 || *upd.StampCost > MaxStampCost {
			return nil, fmt.Errorf("lxmf: stamp cost must be 0 to %d", MaxStampCost)
		}
		fields["stamp_cost"] = strconv.Itoa(*upd.StampCost)
		changes = append(changes, "stamp cost "+strconv.Itoa(*upd.StampCost))
	}
	if upd.Enabled != nil {
		fields["enabled"] = strconv.FormatBool(*upd.Enabled)
		changes = append(changes, "enabled "+strconv.FormatBool(*upd.Enabled))
	}
	if upd.PropagationEnabled != nil {
		fields["propagation_enabled"] = strconv.FormatBool(*upd.PropagationEnabled)
		changes = append(changes, "propagation "+strconv.FormatBool(*upd.PropagationEnabled))
	}
	if upd.PropagationStampCost != nil {
		if *upd.PropagationStampCost < 0 || *upd.PropagationStampCost > MaxStampCost {
			return nil, fmt.Errorf("lxmf: propagation stamp cost must be 0 to %d", MaxStampCost)
		}
		fields["propagation_stamp_cost"] = strconv.Itoa(*upd.PropagationStampCost)
		changes = append(changes, "propagation stamp cost "+strconv.Itoa(*upd.PropagationStampCost))
	}
	if len(fields) == 0 {
		return s.LoadIdentity(ctx, tenantID)
	}
	if _, err := s.accounts.Set(ctx, tenantID, integrations.ProviderLXMF, fields); err != nil {
		return nil, err
	}
	s.auditLog(ctx, tenantID, "lxmf_identity_updated", actor, strings.Join(changes, ", "))
	return s.LoadIdentity(ctx, tenantID)
}
