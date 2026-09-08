package leader

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1 "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/meshsat/meshsat-hub/internal/metrics"
)

// configRetryInterval is how long KubeLease waits before retrying the
// in-cluster configuration after a failure. A variable so tests can shorten it.
var configRetryInterval = 30 * time.Second

// KubeLease implements Leader using the Kubernetes Lease API.
// Used in kubernetes mode for leader election without NATS dependency.
//
// When the in-cluster configuration or client cannot be created, the instance
// is NEVER leader: it logs the error, exports it through LastError and retries
// periodically. The previous behaviour (fall back to "always leader") made every
// replica a leader at once, which is exactly the double-dispatch class of bug
// leader election exists to prevent.
type KubeLease struct {
	instanceID string
	namespace  string
	leaseName  string
	isLeading  atomic.Bool

	mu      sync.Mutex
	lastErr error
}

// NewKubeLease creates a Kubernetes Lease-based leader elector.
// instanceID should be unique per pod (e.g., hostname or pod name).
func NewKubeLease(instanceID string) *KubeLease {
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		ns = "default"
	}
	return &KubeLease{
		instanceID: instanceID,
		namespace:  ns,
		leaseName:  "meshsat-hub-leader",
	}
}

// Run starts the Kubernetes leader election loop. Blocks until ctx is cancelled.
func (k *KubeLease) Run(ctx context.Context, onAcquired func(), onLost func()) {
	metrics.LeaderStatus.WithLabelValues("kubelease").Set(0)

	// Only the coordination API group is needed for a Lease lock; the typed
	// client keeps the binary ~30 MiB smaller than the full clientset.
	var client coordinationv1.CoordinationV1Interface
	for {
		cfg, err := rest.InClusterConfig()
		if err == nil {
			client, err = coordinationv1.NewForConfig(cfg)
		}
		if err == nil {
			k.setErr(nil)
			break
		}
		k.setErr(err)
		slog.Error("leader: k8s lease election unavailable, this instance is NOT leader",
			"error", err, "retry_in", configRetryInterval)
		t := time.NewTimer(configRetryInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      k.leaseName,
			Namespace: k.namespace,
		},
		Client: client,
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: k.instanceID,
		},
	}

	slog.Info("leader: k8s lease election starting",
		"instance", k.instanceID,
		"namespace", k.namespace,
		"lease", k.leaseName,
	)

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				slog.Info("leader: acquired k8s lease leadership")
				k.isLeading.Store(true)
				metrics.LeaderStatus.WithLabelValues("kubelease").Set(1)
				onAcquired()
				<-ctx.Done()
			},
			OnStoppedLeading: func() {
				slog.Info("leader: lost k8s lease leadership")
				k.isLeading.Store(false)
				metrics.LeaderStatus.WithLabelValues("kubelease").Set(0)
				onLost()
			},
			OnNewLeader: func(identity string) {
				if identity != k.instanceID {
					slog.Info("leader: new leader elected", "leader", identity)
				}
			},
		},
	})
}

// IsLeader returns true if this instance currently holds the Kubernetes Lease.
func (k *KubeLease) IsLeader() bool {
	return k.isLeading.Load()
}

// LastError returns the most recent election setup error, or nil once the
// election loop is running. Exposed for the readiness detail view.
func (k *KubeLease) LastError() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.lastErr
}

func (k *KubeLease) setErr(err error) {
	k.mu.Lock()
	k.lastErr = err
	k.mu.Unlock()
}

// Compile-time check.
var _ Leader = (*KubeLease)(nil)
