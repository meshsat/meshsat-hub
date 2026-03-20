package reticulum

import "context"

// BackendAdapter wraps send/availability functions as a SatelliteSender.
// This avoids importing the constellation package and works around Go's
// lack of covariant return types in interface matching.
type BackendAdapter struct {
	sendFn     func(ctx context.Context, deviceID string, payload []byte) error
	availFn    func(ctx context.Context) bool
	maxPayload int
	costPerMsg float64
}

// NewBackendAdapter creates a SatelliteSender from individual functions.
func NewBackendAdapter(
	sendFn func(ctx context.Context, deviceID string, payload []byte) error,
	availFn func(ctx context.Context) bool,
	maxPayload int,
	costPerMsg float64,
) *BackendAdapter {
	return &BackendAdapter{
		sendFn:     sendFn,
		availFn:    availFn,
		maxPayload: maxPayload,
		costPerMsg: costPerMsg,
	}
}

func (a *BackendAdapter) Send(ctx context.Context, deviceID string, payload []byte) error {
	return a.sendFn(ctx, deviceID, payload)
}

func (a *BackendAdapter) IsAvailable(ctx context.Context) bool {
	return a.availFn(ctx)
}

func (a *BackendAdapter) MaxPayload() int         { return a.maxPayload }
func (a *BackendAdapter) CostPerMessage() float64 { return a.costPerMsg }

// Compile-time check.
var _ SatelliteSender = (*BackendAdapter)(nil)
