package takoperator

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func renderedInstance(t *testing.T) corev1.PodSpec {
	t.Helper()
	dep := InstanceDeployment("abcdefghij", "ots@sha256:x", "rabbit:1", "nginx@sha256:y", 1)
	return dep.Spec.Template.Spec
}

func containerNames(cs []corev1.Container) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func findContainer(cs []corev1.Container, name string) *corev1.Container {
	for i := range cs {
		if cs[i].Name == name {
			return &cs[i]
		}
	}
	return nil
}

// RabbitMQ must be a NATIVE SIDECAR, not an ordinary container.
//
// OpenTAKServer's API and its CoT parser both connect to the broker while
// starting. Ordinary containers start in parallel, and in the phase-2 gate that
// cost two crashes: ots-api died with pika.exceptions.AMQPConnectionError and
// ots-cot exited 0, both because nothing was listening yet. They recovered only
// because the kubelet restarted them.
//
// As an init container with restartPolicy: Always it is started and ready before
// any main container runs. If a future edit moves it back among the containers,
// the races return silently — the instance still works, just unreliably at
// startup — so this asserts the shape rather than the behaviour.
func TestRabbitMQIsANativeSidecarSoTheOTSProcessesNeverRaceIt(t *testing.T) {
	spec := renderedInstance(t)

	rabbit := findContainer(spec.InitContainers, "rabbitmq")
	if rabbit == nil {
		t.Fatalf("rabbitmq is not an init container; init containers are %v", containerNames(spec.InitContainers))
	}
	if rabbit.RestartPolicy == nil || *rabbit.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("rabbitmq restartPolicy = %v, want Always — without it this is a plain init "+
			"container that must EXIT before the others start, and the broker would never be up",
			rabbit.RestartPolicy)
	}
	// A STARTUP probe is what orders startup, and this distinction cost a whole
	// gate run. For a native sidecar the kubelet starts the main containers once
	// the sidecar has STARTED; only a startupProbe makes it wait for the sidecar
	// to be usable. A readinessProbe governs the POD's readiness instead.
	//
	// With readiness alone, rabbitmq and ots-eud started in the same second,
	// ots-eud raced the broker, and it then ran with a dead AMQP channel,
	// dropping every phone in close_connection while its own TCP readiness probe
	// kept passing. Silently broken, which is worse than crash-looping.
	if rabbit.StartupProbe == nil {
		t.Error("the sidecar has no startupProbe; without one the main containers do not wait for " +
			"the broker, and ots-eud comes up with a dead AMQP channel that still looks ready")
	}
	if rabbit.ReadinessProbe == nil {
		t.Error("the sidecar should also carry a readiness probe, so a broker that dies later " +
			"takes the pod out of service instead of leaving it advertised as healthy")
	}
	if findContainer(spec.Containers, "rabbitmq") != nil {
		t.Errorf("rabbitmq is ALSO a main container; it would start twice. containers=%v",
			containerNames(spec.Containers))
	}

	// The config init container must still run to completion, and must come
	// before the sidecar so the data folder is seeded first.
	cfg := findContainer(spec.InitContainers, "config")
	if cfg == nil {
		t.Fatal("the config init container is missing")
	}
	if cfg.RestartPolicy != nil {
		t.Error("the config init container must NOT be a sidecar; it has to finish before anything else starts")
	}
	if spec.InitContainers[0].Name != "config" {
		t.Errorf("init container order is %v; config must be first so the CA and config.yml exist",
			containerNames(spec.InitContainers))
	}
}

// The CoT listener is the one container whose readiness a customer can feel, and
// the one whose absence of a probe produced a false Ready: with no probe a
// container counts as ready as soon as it is running, so ots-eud reported ready
// between crash loops while port 8089 refused every connection.
func TestTheCoTListenerHasAReadinessProbeOnItsOwnPort(t *testing.T) {
	spec := renderedInstance(t)
	eud := findContainer(spec.Containers, "ots-eud")
	if eud == nil {
		t.Fatalf("ots-eud is missing; containers are %v", containerNames(spec.Containers))
	}
	if eud.ReadinessProbe == nil {
		t.Fatal("ots-eud has no readiness probe, so 'running' will again be mistaken for 'serving'")
	}
	probe := eud.ReadinessProbe.TCPSocket
	if probe == nil {
		t.Fatal("the probe must be a TCP check: the CoT port speaks TLS with a client certificate, not HTTP")
	}
	if got := probe.Port.IntValue(); got != int(EUDPort) {
		t.Errorf("probe port = %d, want the CoT port %d", got, EUDPort)
	}
}

// Everything in this namespace runs under PodSecurity `restricted`, which the
// namespace enforces. A container that quietly loses one of these fields is
// rejected at admission, which surfaces as an instance that never starts.
func TestEveryContainerSatisfiesTheRestrictedProfile(t *testing.T) {
	spec := renderedInstance(t)
	all := append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...)
	if len(all) < 6 {
		t.Fatalf("expected the config init, the rabbitmq sidecar and four containers, got %d", len(all))
	}
	for _, c := range all {
		t.Run(c.Name, func(t *testing.T) {
			sc := c.SecurityContext
			if sc == nil {
				t.Fatal("no securityContext")
			}
			if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
				t.Error("runAsNonRoot must be true")
			}
			if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
				t.Error("allowPrivilegeEscalation must be false")
			}
			if sc.Capabilities == nil || len(sc.Capabilities.Drop) == 0 || sc.Capabilities.Drop[0] != "ALL" {
				t.Error("all capabilities must be dropped")
			}
			if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
				t.Error("seccompProfile must be RuntimeDefault")
			}
			if c.Resources.Limits.Memory().IsZero() {
				t.Error("a memory limit is the price of the control-plane toleration")
			}
			if !c.Resources.Limits.Cpu().IsZero() {
				t.Error("no CPU limits here: the house rule is requests only, because a limit throttles rather than protects")
			}
		})
	}
}

// Third-party Python on a public path must not hold an API credential, and the
// instance must never acquire node-local state that no backup covers.
func TestTheInstancePodHoldsNoTokenAndNoVolumeClaim(t *testing.T) {
	spec := renderedInstance(t)
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken must be false on the pod as well as the ServiceAccount")
	}
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			t.Errorf("volume %q is a PersistentVolumeClaim; all durable state belongs in the TAK Postgres cluster", v.Name)
		}
		if v.HostPath != nil {
			t.Errorf("volume %q is a hostPath, which `restricted` forbids and which would reach the node", v.Name)
		}
	}
	if spec.PriorityClassName != "meshsat-tak" {
		t.Errorf("priorityClassName = %q, want meshsat-tak so a customer's map server cannot outrank the Hub's data path",
			spec.PriorityClassName)
	}
	// The CA volume must project the certificate only: the wrapped key lives in
	// the same Secret and the pod must be able to verify phones, never mint them.
	for _, v := range spec.Volumes {
		if v.Name != "ca" || v.Secret == nil {
			continue
		}
		if len(v.Secret.Items) == 0 {
			t.Fatal("the ca volume projects the whole Secret, which would hand the pod the wrapped CA key")
		}
		for _, it := range v.Secret.Items {
			if it.Key == "ca.key.enc" {
				t.Error("the ca volume projects ca.key.enc; the instance must never see the CA key")
			}
		}
	}
}
