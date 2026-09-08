package constellation

import (
	"context"
	"fmt"

	"github.com/meshsat/meshsat-hub/internal/cloudloop"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// IridiumBackend wraps the Cloudloop client as a constellation Backend.
// Uses the official Cloudloop Data API (SendSBD) for MT message delivery.
type IridiumBackend struct {
	client   *cloudloop.Client
	resolver cloudloop.DeviceResolver
	pool     *cloudloop.ClientPool // per-tenant accounts (MESHSAT-977); nil = client only
	tenants  *tenancy.Resolver
}

// SetClientPool makes sends use the device tenant's Cloudloop account.
func (b *IridiumBackend) SetClientPool(p *cloudloop.ClientPool) { b.pool = p }

// SetTenants attaches the device -> tenant resolver.
func (b *IridiumBackend) SetTenants(r *tenancy.Resolver) { b.tenants = r }

func (b *IridiumBackend) tenantOf(ctx context.Context, deviceID string) string {
	if b.tenants == nil {
		return store.DefaultTenantID
	}
	return b.tenants.ForDevice(ctx, deviceID)
}

// NewIridiumBackend creates an Iridium backend from an existing Cloudloop client.
func NewIridiumBackend(client *cloudloop.Client) *IridiumBackend {
	return &IridiumBackend{client: client}
}

// SetDeviceResolver attaches a resolver for IMEI → thingID + protocol lookup.
func (b *IridiumBackend) SetDeviceResolver(r cloudloop.DeviceResolver) {
	b.resolver = r
}

func (b *IridiumBackend) Name() string { return "iridium" }

func (b *IridiumBackend) Send(ctx context.Context, deviceID string, payload []byte) (*SendResult, error) {
	tenant := b.tenantOf(ctx, deviceID)
	client := b.client
	if b.pool != nil {
		client = b.pool.ForTenant(ctx, tenant)
	}
	if client == nil {
		return nil, fmt.Errorf("no Cloudloop account configured for tenant %s", tenant)
	}
	thingID := deviceID
	isIMT := false
	if b.resolver != nil {
		thingID, isIMT = b.resolver.Resolve(tenant, deviceID)
	}

	var resp *cloudloop.MTResponse
	var err error
	if isIMT {
		resp, err = client.SendIMT(ctx, thingID, payload, "", "")
	} else {
		resp, err = client.SendSBD(ctx, thingID, payload)
	}
	if err != nil {
		return nil, err
	}
	return &SendResult{
		ID:     resp.ID,
		Status: resp.Status,
		Error:  resp.Error,
	}, nil
}

func (b *IridiumBackend) CheckStatus(ctx context.Context, sendID string) (*SendResult, error) {
	resp, err := b.client.GetDeliveryStatus(ctx, sendID)
	if err != nil {
		return nil, err
	}
	return &SendResult{
		ID:     resp.ID,
		Status: resp.Status,
		Error:  resp.Error,
	}, nil
}

func (b *IridiumBackend) IsAvailable(ctx context.Context) bool {
	return b.client.IsReachable(ctx)
}

func (b *IridiumBackend) MaxPayload() int         { return 270 } // MT buffer
func (b *IridiumBackend) CostPerMessage() float64 { return 0.05 }

// Compile-time check.
var _ Backend = (*IridiumBackend)(nil)
