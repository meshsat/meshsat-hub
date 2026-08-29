package constellation

import (
	"context"

	"github.com/meshsat/meshsat-hub/internal/cloudloop"
)

// IridiumBackend wraps the Cloudloop client as a constellation Backend.
// Uses the official Cloudloop Data API (SendSBD) for MT message delivery.
type IridiumBackend struct {
	client   *cloudloop.Client
	resolver cloudloop.DeviceResolver
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
	thingID := deviceID
	isIMT := false
	if b.resolver != nil {
		thingID, isIMT = b.resolver.Resolve(deviceID)
	}

	var resp *cloudloop.MTResponse
	var err error
	if isIMT {
		resp, err = b.client.SendIMT(ctx, thingID, payload, "", "")
	} else {
		resp, err = b.client.SendSBD(ctx, thingID, payload)
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
