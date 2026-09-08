// Package leader provides leader election for singleton services (TAK, APRS-IS).
// Implementations: Noop (standalone), NATS (cluster), Kube (k8s).
package leader

import (
	"context"
	"log/slog"

	"github.com/meshsat/meshsat-hub/internal/metrics"
)

// Leader manages leader election for singleton services.
type Leader interface {
	// Run starts leader election. Calls onAcquired when this instance becomes
	// leader, and onLost when leadership is lost. Blocks until ctx is cancelled.
	Run(ctx context.Context, onAcquired func(), onLost func())

	// IsLeader returns true if this instance currently holds leadership.
	IsLeader() bool
}

// Noop always considers itself the leader. Used in standalone mode
// where there's only one instance.
type Noop struct{}

func NewNoop() *Noop { return &Noop{} }

func (n *Noop) Run(ctx context.Context, onAcquired func(), onLost func()) {
	slog.Info("leader: standalone mode — always leader")
	metrics.LeaderStatus.WithLabelValues("noop").Set(1)
	onAcquired()
	<-ctx.Done()
	metrics.LeaderStatus.WithLabelValues("noop").Set(0)
	onLost()
}

func (n *Noop) IsLeader() bool { return true }

// Compile-time check.
var _ Leader = (*Noop)(nil)
