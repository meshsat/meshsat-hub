package constellation

import (
	"context"

	"github.com/meshsat/meshsat-hub/internal/globalstar"
)

// GlobalstarBackend wraps the Globalstar client as a constellation Backend.
type GlobalstarBackend struct {
	client *globalstar.Client
}

// NewGlobalstarBackend creates a Globalstar backend from an existing client.
func NewGlobalstarBackend(client *globalstar.Client) *GlobalstarBackend {
	return &GlobalstarBackend{client: client}
}

func (b *GlobalstarBackend) Name() string { return "globalstar" }

func (b *GlobalstarBackend) Send(ctx context.Context, deviceID string, payload []byte) (*SendResult, error) {
	resp, err := b.client.SendMessage(ctx, deviceID, payload)
	if err != nil {
		return nil, err
	}
	return &SendResult{
		ID:     resp.ID,
		Status: resp.Status,
		Error:  resp.Error,
	}, nil
}

func (b *GlobalstarBackend) CheckStatus(ctx context.Context, sendID string) (*SendResult, error) {
	status, err := b.client.CheckMessageStatus(ctx, sendID)
	if err != nil {
		return nil, err
	}
	return &SendResult{
		ID:     status.ID,
		Status: status.Status,
		Error:  status.Error,
	}, nil
}

func (b *GlobalstarBackend) IsAvailable(ctx context.Context) bool {
	return b.client.IsReachable(ctx)
}

func (b *GlobalstarBackend) MaxPayload() int         { return globalstar.MaxPayloadBytes } // 128 bytes
func (b *GlobalstarBackend) CostPerMessage() float64 { return globalstar.DefaultCostPerMessage }

// Compile-time check.
var _ Backend = (*GlobalstarBackend)(nil)
