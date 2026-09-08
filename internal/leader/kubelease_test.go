package leader

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestKubeLeaseNeverLeaderWithoutCluster is the regression test for the old
// "fall back to always leader" path: outside a cluster the elector must not
// grant leadership.
func TestKubeLeaseNeverLeaderWithoutCluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	orig := configRetryInterval
	configRetryInterval = 20 * time.Millisecond
	t.Cleanup(func() { configRetryInterval = orig })

	k := NewKubeLease("test-1")
	var acquired, lost atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		k.Run(ctx, func() { acquired.Add(1) }, func() { lost.Add(1) })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
	if acquired.Load() != 0 || lost.Load() != 0 {
		t.Fatalf("callbacks fired without a cluster: acquired=%d lost=%d", acquired.Load(), lost.Load())
	}
	if k.IsLeader() {
		t.Fatal("IsLeader must be false without a cluster")
	}
	if k.LastError() == nil {
		t.Fatal("LastError should report the in-cluster config failure")
	}
}

func TestKubeLeaseNamespaceDefault(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	if k := NewKubeLease("x"); k.namespace != "default" {
		t.Errorf("namespace = %q, want default", k.namespace)
	}
	t.Setenv("POD_NAMESPACE", "meshsat-hub")
	if k := NewKubeLease("x"); k.namespace != "meshsat-hub" {
		t.Errorf("namespace = %q, want meshsat-hub", k.namespace)
	}
}
