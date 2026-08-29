package leader

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
)

// NATSLeader uses MQTT exclusive subscription for leader election.
// Only one subscriber in the queue group receives the heartbeat — that's the leader.
// Used in cluster mode (no k8s API needed).
type NATSLeader struct {
	bus       bus.MessageBus
	topic     string
	group     string
	isLeading atomic.Bool
}

// NewNATS creates a NATS-based leader elector.
func NewNATS(messageBus bus.MessageBus, instanceID string) *NATSLeader {
	return &NATSLeader{
		bus:   messageBus,
		topic: "meshsat/hub/leader/heartbeat",
		group: "meshsat-hub-leaders",
	}
}

// Run starts the leader election loop. Publishes heartbeats and subscribes
// with a queue group — only one instance in the group processes each heartbeat.
func (l *NATSLeader) Run(ctx context.Context, onAcquired func(), onLost func()) {
	slog.Info("leader: NATS election starting", "topic", l.topic, "group", l.group)

	// Subscribe to heartbeat with queue group — only one instance gets each message
	err := l.bus.QueueSubscribe(l.topic, 1, l.group, func(topic string, payload []byte) {
		if !l.isLeading.Load() {
			l.isLeading.Store(true)
			slog.Info("leader: acquired leadership")
			onAcquired()
		}
	})
	if err != nil {
		slog.Error("leader: subscribe failed, running as standalone", "error", err)
		l.isLeading.Store(true)
		onAcquired()
		<-ctx.Done()
		onLost()
		return
	}

	// Publish heartbeats — the instance that receives its own heartbeat is the leader
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	lostTimer := time.NewTimer(15 * time.Second)
	defer lostTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			if l.isLeading.Load() {
				l.isLeading.Store(false)
				onLost()
			}
			return
		case <-ticker.C:
			_ = l.bus.Publish(l.topic, 1, false, []byte("ping"))
		case <-lostTimer.C:
			// If we haven't received a heartbeat in 15s, we lost leadership
			if l.isLeading.Load() {
				l.isLeading.Store(false)
				slog.Warn("leader: leadership lost (no heartbeat)")
				onLost()
			}
			lostTimer.Reset(15 * time.Second)
		}
	}
}

func (l *NATSLeader) IsLeader() bool {
	return l.isLeading.Load()
}

// Compile-time check.
var _ Leader = (*NATSLeader)(nil)
