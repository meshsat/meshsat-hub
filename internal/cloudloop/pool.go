package cloudloop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// ClientPool hands out one Cloudloop API client per tenant account
// (MESHSAT-977). The default tenant gets the platform client when it has no
// account of its own; every other tenant needs its own account.
type ClientPool struct {
	platform *Client
	accounts *integrations.Service

	mu      sync.Mutex
	clients map[string]*pooled // tenant -> client for the account fingerprint
}

type pooled struct {
	fp     string
	client *Client
}

// NewClientPool creates a pool. platform may be nil (no environment account);
// accounts may be nil (single-tenant mode: everyone gets platform).
func NewClientPool(platform *Client, accounts *integrations.Service) *ClientPool {
	return &ClientPool{platform: platform, accounts: accounts, clients: map[string]*pooled{}}
}

// ForTenant returns the client for a tenant, or nil when the tenant has no
// Cloudloop account (and is not the default tenant with a platform account).
func (p *ClientPool) ForTenant(ctx context.Context, tenantID string) *Client {
	if p == nil {
		return nil
	}
	if tenantID == "" {
		tenantID = store.DefaultTenantID
	}
	if p.accounts == nil {
		return p.platform
	}
	acct, err := p.accounts.ForTenant(ctx, tenantID, integrations.ProviderCloudloop)
	if err != nil {
		slog.Warn("cloudloop: account lookup failed", "tenant", tenantID, "error", err)
		return nil
	}
	if acct == nil {
		return nil
	}
	if acct.Platform && p.platform != nil {
		return p.platform
	}
	fp := fingerprint(acct.Get("api_url"), acct.Get("api_key"))
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[tenantID]; ok && c.fp == fp {
		return c.client
	}
	c := NewClient(acct.Get("api_url"), acct.Get("api_key"))
	p.clients[tenantID] = &pooled{fp: fp, client: c}
	return c
}

// Tenants lists the tenants that have a Cloudloop account (the default tenant
// included when a platform account exists).
func (p *ClientPool) Tenants(ctx context.Context) []string {
	if p == nil {
		return nil
	}
	if p.accounts == nil {
		if p.platform != nil {
			return []string{store.DefaultTenantID}
		}
		return nil
	}
	ts, err := p.accounts.TenantsWith(ctx, integrations.ProviderCloudloop)
	if err != nil {
		slog.Warn("cloudloop: listing tenant accounts failed", "error", err)
		return nil
	}
	return ts
}

func fingerprint(parts ...string) string {
	h := sha256.New()
	for _, s := range parts {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
