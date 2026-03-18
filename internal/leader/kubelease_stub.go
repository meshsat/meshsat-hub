//go:build !kubernetes

package leader

import "log/slog"

// NewKubeLease returns a Noop leader when built without the kubernetes tag.
// To enable real Kubernetes Lease election, build with: go build -tags kubernetes
func NewKubeLease(instanceID string) *Noop {
	slog.Warn("leader: kubernetes build tag not set, falling back to noop (always leader)")
	return NewNoop()
}
