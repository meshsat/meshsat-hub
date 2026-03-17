package constellation

import (
	"context"

	"github.com/cubeos-app/meshsat-hub/internal/cloudloop"
)

// IridiumBackend wraps the Cloudloop client as a constellation Backend.
type IridiumBackend struct {
	client *cloudloop.Client
}

// NewIridiumBackend creates an Iridium backend from an existing Cloudloop client.
func NewIridiumBackend(client *cloudloop.Client) *IridiumBackend {
	return &IridiumBackend{client: client}
}

func (b *IridiumBackend) Name() string { return "iridium" }

func (b *IridiumBackend) Send(ctx context.Context, deviceID string, payload []byte) (*SendResult, error) {
	resp, err := b.client.SendMT(ctx, deviceID, payload)
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
	resp, err := b.client.CheckMTStatus(ctx, sendID)
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
