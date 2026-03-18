package leader

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// KubeLease implements Leader using the Kubernetes Lease API.
// Used in kubernetes mode for leader election without NATS dependency.
type KubeLease struct {
	instanceID string
	namespace  string
	leaseName  string
	isLeading  atomic.Bool
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
	cfg, err := rest.InClusterConfig()
	if err != nil {
		slog.Error("leader: k8s in-cluster config failed, falling back to standalone", "error", err)
		k.isLeading.Store(true)
		onAcquired()
		<-ctx.Done()
		onLost()
		return
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		slog.Error("leader: k8s client creation failed, falling back to standalone", "error", err)
		k.isLeading.Store(true)
		onAcquired()
		<-ctx.Done()
		onLost()
		return
	}

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      k.leaseName,
			Namespace: k.namespace,
		},
		Client: client.CoordinationV1(),
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
				onAcquired()
				<-ctx.Done()
			},
			OnStoppedLeading: func() {
				slog.Info("leader: lost k8s lease leadership")
				k.isLeading.Store(false)
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

// Compile-time check.
var _ Leader = (*KubeLease)(nil)
