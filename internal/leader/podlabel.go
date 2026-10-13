package leader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

// PodLabeler marks and unmarks the running pod with a label, so that a
// Service can select the one replica that holds a leader-only listener
// (MESHSAT-1555: the Reticulum node runs on the leader, and a Service that
// balanced across both pods would refuse half of all connections).
//
// Only the pod's own labels are touched, by a JSON merge patch on the one
// pod named by POD_NAME in POD_NAMESPACE; the Role grants get and patch on
// pods in the namespace, which is the narrowest shape Kubernetes RBAC has
// for "this pod" (resourceNames cannot name a pod that does not exist yet).
type PodLabeler struct {
	namespace, pod string
	client         corev1.CoreV1Interface
}

// NewPodLabeler builds a labeler for the running pod from the in-cluster
// configuration. It fails when the pod does not know its own name or the
// API is unreachable; the caller then runs without a leader-following
// Service, which standalone and cluster modes do by design.
func NewPodLabeler() (*PodLabeler, error) {
	pod, ns := os.Getenv("POD_NAME"), os.Getenv("POD_NAMESPACE")
	if pod == "" || ns == "" {
		return nil, errors.New("leader: POD_NAME and POD_NAMESPACE are not set")
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("leader: in-cluster config: %w", err)
	}
	client, err := corev1.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("leader: core client: %w", err)
	}
	return &PodLabeler{namespace: ns, pod: pod, client: client}, nil
}

// Set puts key=value on the pod's labels.
func (p *PodLabeler) Set(ctx context.Context, key, value string) error {
	return p.patch(ctx, map[string]*string{key: &value})
}

// Clear removes key from the pod's labels. Clearing a label the pod does not
// carry is a no-op, so this is safe to run at every start: a pod that
// restarted in place keeps its labels, and a stale leader mark would make
// the Service send connections to a replica with no listener.
func (p *PodLabeler) Clear(ctx context.Context, key string) error {
	return p.patch(ctx, map[string]*string{key: nil})
}

func (p *PodLabeler) patch(ctx context.Context, labels map[string]*string) error {
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	if err != nil {
		return err
	}
	_, err = p.client.Pods(p.namespace).Patch(ctx, p.pod, types.MergePatchType, body, metav1.PatchOptions{})
	return err
}
