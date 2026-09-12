package takoperator

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Rendering one tenant's OpenTAKServer.
//
// The shape below is the one spike S5 validated on this cluster, hardened to what
// the namespace now enforces. Five containers and an init container, all from two
// images, because OpenTAKServer is not one process: the API, the EUD handler and
// the CoT parser are separate programs that talk through RabbitMQ, and the
// RabbitMQ they talk through has to be in the pod, since OpenTAKServer's socketio
// client connects as `guest` with no credentials and guest is loopback-only.
//
// The typed k8s.io/api structs are used deliberately for this, while the client
// that writes it stays hand-rolled JSON. internal/bridge/casecret.go avoids the
// typed CLIENTS because their machinery costs tens of megabytes; the API types
// are a fraction of that, and a 200-line pod spec built from maps is a spec where
// a typo in a field name is a runtime surprise instead of a compile error.
//
// Everything here is governed by PodSecurity `restricted`, which the namespace
// enforces: non-root, no privilege escalation, all capabilities dropped, a
// RuntimeDefault seccomp profile on every container. That is not decoration —
// each of these pods runs third-party Python reachable from the internet through
// the Hub, on the same kernel as etcd.

// UID is the user every OpenTAKServer container runs as, matching the image.
const UID int64 = 10001

// rabbitMQ runs as its own image's user: uid 100, gid 101 on alpine. As root
// with capabilities dropped its entrypoint fails trying to chown the data
// directory; as non-root it skips the chown, and the pod's fsGroup still lets it
// write the emptyDir. Found in spike S5.
const (
	rabbitUID int64 = 100
	rabbitGID int64 = 101
)

// EUDPort is the CoT streaming port the Hub connects to, and AdminPort is the
// narrow admin gateway. Neither is reachable from outside the cluster: the Hub is
// an instance's only client.
const (
	EUDPort   int32 = 8089
	AdminPort int32 = 8444
)

func objectLabels(label string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "tak-instance",
		"app.kubernetes.io/part-of":   "meshsat-hub",
		"app.kubernetes.io/component": "opentakserver",
		// The opaque label, never the tenant id: a Kubernetes label is visible
		// to anybody who can list pods in the namespace.
		"tak.meshsat.net/label": label,
	}
}

func qty(s string) resource.Quantity { return resource.MustParse(s) }

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(i int64) *int64 { return &i }
func int32Ptr(i int32) *int32 { return &i }

// restricted is the security context every container here carries.
func restricted(uid, gid int64) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             boolPtr(true),
		RunAsUser:                int64Ptr(uid),
		RunAsGroup:               int64Ptr(gid),
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// InstanceDeployment renders one tenant's OpenTAKServer.
//
// replicas is 0 when the instance is suspended or hibernated, which is how
// "stop serving" is expressed: nothing is deleted, and the data is untouched.
//
// The strategy is Recreate, not RollingUpdate. Two OpenTAKServers against one
// database would both run migrations and both bind the same RabbitMQ queues; a
// few seconds of downtime during an upgrade is the cheaper failure.
func InstanceDeployment(label, otsImage, rabbitImage, nginxImage string, replicas int32) appsv1.Deployment {
	name := InstanceName(label)
	labels := objectLabels(label)
	dataMount := corev1.VolumeMount{Name: "data", MountPath: "/var/lib/ots"}
	otsEnv := []corev1.EnvVar{{Name: "OTS_DATA_FOLDER", Value: "/var/lib/ots"}}

	otsContainer := func(cname string, command []string, mem string) corev1.Container {
		return corev1.Container{
			Name:            cname,
			Image:           otsImage,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         command,
			Env:             otsEnv,
			SecurityContext: restricted(UID, UID),
			VolumeMounts:    []corev1.VolumeMount{dataMount},
			Resources: corev1.ResourceRequirements{
				// Requests from the phase-0 measurement of a real instance at
				// idle; limits are a ceiling, and there is deliberately no CPU
				// limit (CoT parsing is bursty and a limit throttles rather than
				// protects).
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    qty("50m"),
					corev1.ResourceMemory: qty(mem),
				},
				Limits: corev1.ResourceList{corev1.ResourceMemory: qty("1Gi")},
			},
		}
	}

	return appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: DefaultNamespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(replicas),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"app.kubernetes.io/name": "tak-instance",
				"tak.meshsat.net/label":  label,
			}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: "meshsat-tak",
					// Third-party Python has no business holding an API
					// credential, and this is belt as well as braces: the
					// ServiceAccount sets it too.
					AutomountServiceAccountToken: boolPtr(false),
					EnableServiceLinks:           boolPtr(false),
					PriorityClassName:            "meshsat-tak",
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   boolPtr(true),
						FSGroup:        int64Ptr(UID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					// The owner accepted internet-facing third-party Python on
					// the control-plane tier, because the workers are 49, 90 and
					// 92 percent committed. Recorded in k8s/CONVENTIONS.md with
					// the controls that are its price.
					NodeSelector: map[string]string{"node-role.kubernetes.io/control-plane": ""},
					Tolerations: []corev1.Toleration{{
						Key:      "node-role.kubernetes.io/control-plane",
						Operator: corev1.TolerationOpExists,
						Effect:   corev1.TaintEffectNoSchedule,
					}},
					Volumes: []corev1.Volume{
						// Stateless on disk: all durable state is in the TAK
						// Postgres cluster, icons are baked into the image, and
						// the namespace's quota forbids PersistentVolumeClaims so
						// this cannot quietly become node-local state nothing
						// backs up.
						{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "rabbit", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "ca", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: CASecretName(label),
							// The CA certificate and the server certificate
							// only. The WRAPPED CA key is in the same Secret and
							// is deliberately not projected: the pod must be
							// able to verify phones, never to mint them. Spike
							// S5 proved OpenTAKServer runs with no CA key on
							// disk.
							Items: []corev1.KeyToPath{{Key: "ca.pem", Path: "ca.pem"}},
						}}},
						{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: TLSSecretName(label),
						}}},
						{Name: "nginx", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: ConfigSecretName(label),
							Items:      []corev1.KeyToPath{{Key: "nginx.conf", Path: "nginx.conf"}},
						}}},
					},
					InitContainers: []corev1.Container{{
						Name:            "config",
						Image:           otsImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						// The config is written at start rather than mounted,
						// because OpenTAKServer rewrites config.yml at runtime
						// and cannot run against a read-only mount.
						Command:         []string{"python", "-c", configScript},
						SecurityContext: restricted(UID, UID),
						Env: []corev1.EnvVar{
							{Name: "OTS_DATA_FOLDER", Value: "/var/lib/ots"},
							{Name: "TAK_DB_NAME", Value: DatabaseName(label)},
							{Name: "TAK_DB_HOST", Value: DefaultDBCluster + "-rw." + DefaultDBNamespace + ".svc"},
							secretEnv("TAK_DB_USER", DBSecretName(label), corev1.BasicAuthUsernameKey),
							secretEnv("TAK_DB_PASSWORD", DBSecretName(label), corev1.BasicAuthPasswordKey),
							// Pinned once and never regenerated: a new
							// SECURITY_PASSWORD_SALT invalidates every stored
							// password hash, proven in spike S5.
							secretEnv("OTS_PIN_SECRET_KEY", ConfigSecretName(label), "secret_key"),
							secretEnv("OTS_PIN_PASSWORD_SALT", ConfigSecretName(label), "password_salt"),
							secretEnv("OTS_PIN_NODE_ID", ConfigSecretName(label), "node_id"),
							secretEnv("OTS_PIN_CA_PASSWORD", ConfigSecretName(label), "ca_password"),
							secretEnv("OTS_ADMIN_PASSWORD", ConfigSecretName(label), "admin_password"),
						},
						VolumeMounts: []corev1.VolumeMount{
							dataMount,
							{Name: "ca", MountPath: "/seed", ReadOnly: true},
							{Name: "tls", MountPath: "/seed-tls", ReadOnly: true},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: qty("50m"), corev1.ResourceMemory: qty("96Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceMemory: qty("256Mi")},
						},
					}},
					Containers: []corev1.Container{
						{
							Name:            "rabbitmq",
							Image:           rabbitImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Env:             []corev1.EnvVar{{Name: "RABBITMQ_NODENAME", Value: "rabbit@localhost"}},
							SecurityContext: restricted(rabbitUID, rabbitGID),
							VolumeMounts:    []corev1.VolumeMount{{Name: "rabbit", MountPath: "/var/lib/rabbitmq"}},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{corev1.ResourceCPU: qty("50m"), corev1.ResourceMemory: qty("128Mi")},
								Limits:   corev1.ResourceList{corev1.ResourceMemory: qty("512Mi")},
							},
						},
						otsContainer("ots-api", []string{"opentakserver"}, "192Mi"),
						// The EUD handler forks a process per connection, which
						// is why the Hub caps connections per tenant at its own
						// front door rather than relying on this limit.
						otsContainer("ots-eud", []string{"eud_handler", "--ssl"}, "160Mi"),
						otsContainer("ots-cot", []string{"cot_parser"}, "240Mi"),
						{
							Name:            "admin",
							Image:           nginxImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"nginx", "-c", "/etc/nginx/nginx.conf", "-g", "daemon off;"},
							SecurityContext: restricted(UID, UID),
							Ports:           []corev1.ContainerPort{{Name: "admin", ContainerPort: AdminPort}},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "nginx", MountPath: "/etc/nginx", ReadOnly: true},
								{Name: "ca", MountPath: "/etc/tak/ca", ReadOnly: true},
								{Name: "tls", MountPath: "/etc/tak/tls", ReadOnly: true},
								// nginx-unprivileged owns /var/cache/nginx as its
								// own uid; running as another one makes it die on
								// mkdir, with the real cause buried. /tmp is
								// where this config puts every scratch path.
								{Name: "tmp", MountPath: "/tmp"},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{corev1.ResourceCPU: qty("10m"), corev1.ResourceMemory: qty("24Mi")},
								Limits:   corev1.ResourceList{corev1.ResourceMemory: qty("64Mi")},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{
									Port: intstrFromInt(AdminPort),
								}},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
							},
						},
					},
				},
			},
		},
	}
}

func secretEnv(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secret},
			Key:                  key,
		}},
	}
}

// InstanceService is the in-cluster address the Hub dials. ClusterIP, never
// NodePort or LoadBalancer: phones reach the Hub, and the Hub reaches this.
func InstanceService(label string) corev1.Service {
	name := InstanceName(label)
	return corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: DefaultNamespace,
			Labels:    objectLabels(label),
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{
				"app.kubernetes.io/name": "tak-instance",
				"tak.meshsat.net/label":  label,
			},
			Ports: []corev1.ServicePort{
				{Name: "eud", Port: EUDPort, TargetPort: intstrFromInt(EUDPort)},
				{Name: "admin", Port: AdminPort, TargetPort: intstrFromInt(AdminPort)},
			},
		},
	}
}

// ServiceHost is the address written into a TakInstance's status for the Hub to
// dial.
func ServiceHost(label string) string {
	return InstanceName(label) + "." + DefaultNamespace + ".svc:8089"
}
