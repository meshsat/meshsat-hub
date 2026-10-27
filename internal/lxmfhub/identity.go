package lxmfhub

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base32"
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

// Who can reach a tenant's addresses on the Hub's node (MESHSAT-1722).
// Private is the default and what an empty or unknown value means: the
// addresses live on the tenant's own node, reachable only through a
// connection that presents the tenant's network name and passphrase.
// Public puts them on the main node, reachable by anyone on Reticulum. Off
// registers them nowhere.
const (
	AccessPrivate = "private"
	AccessPublic  = "public"
	AccessOff     = "off"
)

// NormalizeAccessMode maps a stored value to a mode; anything unrecognised
// is Private, the restrictive end.
func NormalizeAccessMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case AccessPublic:
		return AccessPublic
	case AccessOff:
		return AccessOff
	}
	return AccessPrivate
}

// newAccessCode generates a Private tenant's network name and passphrase.
// Reticulum derives the interface key from them with no stretching, so the
// passphrase is 32 random bytes, never something a person chose.
func newAccessCode() (netname, passphrase string, err error) {
	n := make([]byte, 4)
	p := make([]byte, 32)
	if _, err := rand.Read(n); err != nil {
		return "", "", err
	}
	if _, err := rand.Read(p); err != nil {
		return "", "", err
	}
	return "meshsat-" + hex.EncodeToString(n), strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(p)), nil
}

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
	// AccessMode is private, public or off (MESHSAT-1722). IFACNetname and
	// IFACPassphrase are what a Private tenant's people enter in their
	// client; the API shows the passphrase to owners and operators only.
	AccessMode     string `json:"access_mode"`
	IFACNetname    string `json:"ifac_netname"`
	IFACPassphrase string `json:"-"`

	ident *rnswire.Identity
}

// IdentityUpdate changes what an owner may change; nil leaves a field alone.
type IdentityUpdate struct {
	DisplayName          *string
	StampCost            *int
	Enabled              *bool
	PropagationEnabled   *bool
	PropagationStampCost *int
	AccessMode           *string
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
	id.AccessMode = NormalizeAccessMode(acct.Get("access_mode"))
	id.IFACNetname, id.IFACPassphrase = acct.Get("ifac_netname"), acct.Get("ifac_passphrase")
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
		netname, pass, err := newAccessCode()
		if err != nil {
			return nil, err
		}
		if _, err := s.accounts.Set(ctx, tenantID, integrations.ProviderLXMF, map[string]string{
			"private_key": PrivateKeyHex(fresh), "display_name": defaultDisplayName(s.tenantName(ctx, tenantID)),
			"stamp_cost": "0", "enabled": "true", "access_mode": AccessPrivate, "ifac_netname": netname, "ifac_passphrase": pass,
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

// UpdateIdentity changes the display name, stamp cost, enabled flag or the
// access mode.
func (s *Service) UpdateIdentity(ctx context.Context, tenantID, actor string, upd IdentityUpdate) (*Identity, error) {
	cur, err := s.LoadIdentity(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	fields := map[string]string{}
	var changes []string
	modeFrom, modeTo := "", ""
	if upd.AccessMode != nil {
		m := strings.ToLower(strings.TrimSpace(*upd.AccessMode))
		if m != AccessPrivate && m != AccessPublic && m != AccessOff {
			return nil, errors.New("lxmf: access mode must be private, public or off")
		}
		if m != cur.AccessMode {
			fields["access_mode"] = m
			modeFrom, modeTo = cur.AccessMode, m
			if m == AccessPrivate && (cur.IFACNetname == "" || cur.IFACPassphrase == "") {
				netname, pass, err := newAccessCode()
				if err != nil {
					return nil, err
				}
				fields["ifac_netname"], fields["ifac_passphrase"] = netname, pass
			}
		}
	}
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
	// Who can reach the address is a security decision of its own on the
	// tenant's chain, apart from the cosmetic settings.
	if modeTo != "" {
		s.auditLog(ctx, tenantID, "lxmf_access_mode_changed", actor, "from "+modeFrom+" to "+modeTo)
	}
	if len(changes) > 0 {
		s.auditLog(ctx, tenantID, "lxmf_identity_updated", actor, strings.Join(changes, ", "))
	}
	return s.LoadIdentity(ctx, tenantID)
}

// RotateAccess replaces a tenant's network name and passphrase. The leader's
// node closes every connection that joined with the old ones when the row's
// change reaches it, so a person who should no longer reach the address is
// out at once; everybody else enters the new values.
func (s *Service) RotateAccess(ctx context.Context, tenantID, actor string) (*Identity, error) {
	if _, err := s.LoadIdentity(ctx, tenantID); err != nil {
		return nil, err
	}
	netname, pass, err := newAccessCode()
	if err != nil {
		return nil, err
	}
	if _, err := s.accounts.Set(ctx, tenantID, integrations.ProviderLXMF, map[string]string{"ifac_netname": netname, "ifac_passphrase": pass}); err != nil {
		return nil, err
	}
	s.auditLog(ctx, tenantID, "lxmf_access_rotated", actor, "new network name "+netname)
	return s.LoadIdentity(ctx, tenantID)
}
