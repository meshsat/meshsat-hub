package constellation

import (
	"context"

	"github.com/cubeos-app/meshsat-hub/internal/astrocast"
)

// AstrocastBackend wraps the Astrocast client as a constellation Backend.
type AstrocastBackend struct {
	client *astrocast.Client
}

// NewAstrocastBackend creates an Astrocast backend from an existing client.
func NewAstrocastBackend(client *astrocast.Client) *AstrocastBackend {
	return &AstrocastBackend{client: client}
}

func (b *AstrocastBackend) Name() string { return "astrocast" }

func (b *AstrocastBackend) Send(ctx context.Context, deviceID string, payload []byte) (*SendResult, error) {
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

func (b *AstrocastBackend) CheckStatus(ctx context.Context, sendID string) (*SendResult, error) {
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

func (b *AstrocastBackend) IsAvailable(ctx context.Context) bool {
	return b.client.IsReachable(ctx)
}

func (b *AstrocastBackend) MaxPayload() int         { return astrocast.MaxPayloadBytes } // 160 bytes
func (b *AstrocastBackend) CostPerMessage() float64 { return astrocast.DefaultCostPerMessage }

// Compile-time check.
var _ Backend = (*AstrocastBackend)(nil)
