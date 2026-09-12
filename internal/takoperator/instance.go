package takoperator

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Finalizer is what stops a TakInstance disappearing before its database, its
// Secrets and its workload have been dealt with. Without it, deleting the object
// would leave a database nobody owns and a CA key nobody can reach.
const Finalizer = "tak.meshsat.net/teardown"

// labelPattern mirrors the CRD's own validation. Checked again here because the
// label becomes part of object names and a database name, and a CRD schema is
// one `kubectl apply` away from being edited.
var labelPattern = regexp.MustCompile(`^[a-z0-9]{10}$`)

// Reconciler turns TakInstances into running OpenTAKServers and
// TakCertificateRequests into certificates.
//
// It is a ticker, not an informer: this namespace holds tens of objects, not
// thousands, and a five-second list is cheaper to reason about than a watch that
// can silently fall behind. Every step is idempotent, so a reconcile that runs
// twice costs nothing.
type Reconciler struct {
	Client      *Client
	Namespace   string
	DBNamespace string
	DBCluster   string

	// Images, pinned by digest in the operator's own config rather than here:
	// the OTS image is not managed by bump_k8s_pin, so it is a deployment
	// decision rather than a build artefact.
	OTSImage    string
	RabbitImage string
	NginxImage  string

	// WrapKey encrypts each tenant CA key at rest. Without it the operator
	// refuses to start: writing an unwrapped CA key into a Secret would put it
	// in etcd unencrypted and in every Velero backup.
	WrapKey []byte

	Log *slog.Logger
}

// Validate checks the reconciler can do its job before it starts doing it
// badly. An operator that starts without a wrap key would generate CA keys it
// cannot protect, and noticing that later means re-enrolling every phone.
func (r *Reconciler) Validate() error {
	switch {
	case r.Client == nil:
		return errors.New("takoperator: no API client")
	case len(r.WrapKey) != 32:
		return fmt.Errorf("%w: operator started without one", ErrWrapKey)
	case r.OTSImage == "", r.RabbitImage == "", r.NginxImage == "":
		return errors.New("takoperator: every image must be configured, pinned by digest")
	case r.Namespace == "":
		return errors.New("takoperator: no namespace")
	}
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.DBNamespace == "" {
		r.DBNamespace = DefaultDBNamespace
	}
	if r.DBCluster == "" {
		r.DBCluster = DefaultDBCluster
	}
	return nil
}

// Run reconciles everything every interval until ctx is done.
//
// One error never stops the loop: a tenant whose database is briefly unreachable
// must not prevent another tenant's instance from being created. Failures land in
// the object's status, where the Hub and a person can both see them.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	r.Log.Info("takoperator: started", "namespace", r.Namespace, "interval", interval.String())
	for {
		if err := r.ReconcileOnce(ctx); err != nil {
			r.Log.Warn("takoperator: reconcile pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			r.Log.Info("takoperator: stopping")
			return
		case <-t.C:
		}
	}
}

// ReconcileOnce makes one pass over both kinds.
func (r *Reconciler) ReconcileOnce(ctx context.Context) error {
	instances, err := r.Client.ListInstances(ctx, r.Namespace)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}
	var firstErr error
	for i := range instances {
		inst := instances[i]
		if err := r.reconcileInstance(ctx, &inst); err != nil {
			r.Log.Warn("takoperator: instance failed", "label", inst.Spec.Label, "error", err)
			r.setFailed(ctx, &inst, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if err := r.reconcileCertRequests(ctx); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (r *Reconciler) reconcileInstance(ctx context.Context, inst *TakInstance) error {
	label := inst.Spec.Label
	if !labelPattern.MatchString(label) {
		// Not retried: a bad label cannot become good, and guessing a valid one
		// would attach a database to the wrong tenant.
		return fmt.Errorf("label %q is not ten lowercase alphanumerics", label)
	}
	if inst.Spec.TenantID == "" {
		return errors.New("instance has no tenantID")
	}

	if inst.DeletionTimestamp != nil {
		return r.teardown(ctx, inst)
	}
	if !hasFinalizer(inst.Finalizers) {
		if err := r.Client.Patch(ctx, InstanceGV, r.Namespace, InstanceResource, inst.Name,
			map[string]any{"metadata": map[string]any{
				"finalizers": append(append([]string{}, inst.Finalizers...), Finalizer),
			}}); err != nil {
			return fmt.Errorf("add finalizer: %w", err)
		}
	}

	ca, err := r.ensureCA(ctx, label)
	if err != nil {
		return err
	}
	if err := r.ensureTLS(ctx, label, ca); err != nil {
		return err
	}
	if err := r.ensureConfig(ctx, label); err != nil {
		return err
	}

	hibernated := inst.Spec.State == StateHibernated
	if err := r.ApplyDatabase(ctx, label, hibernated); err != nil {
		return err
	}

	replicas := int32(1)
	if inst.Spec.State == StateSuspended || hibernated {
		replicas = 0
	}
	dep := InstanceDeployment(label, r.OTSImage, r.RabbitImage, r.NginxImage, replicas)
	if err := r.Client.Apply(ctx, "apps/v1", r.Namespace, "deployments", dep.Name, dep); err != nil {
		return fmt.Errorf("apply deployment: %w", err)
	}
	svc := InstanceService(label)
	if err := r.Client.Apply(ctx, "v1", r.Namespace, "services", svc.Name, svc); err != nil {
		return fmt.Errorf("apply service: %w", err)
	}

	phase := PhaseProvisioning
	switch {
	case inst.Spec.State == StateSuspended:
		phase = PhaseSuspended
	case hibernated:
		phase = PhaseHibernated
	default:
		ready, err := r.deploymentReady(ctx, dep.Name)
		if err != nil {
			return err
		}
		if ready {
			phase = PhaseReady
		}
	}
	return r.Client.SetInstanceStatus(ctx, r.Namespace, inst.Name, TakInstanceStatus{
		Phase:              phase,
		ObservedGeneration: inst.Generation,
		Host:               ServiceHost(label),
		CACertPEM:          string(ca.CertPEM()),
		Conditions: []metav1.Condition{{
			Type:               "Ready",
			Status:             boolToCondition(phase == PhaseReady),
			ObservedGeneration: inst.Generation,
			LastTransitionTime: metav1.Now(),
			Reason:             phase,
		}},
	})
}

// ensureCA loads the tenant's authority, generating it the first time.
//
// The Secret holds the certificate in the clear, because it is a public trust
// anchor, and the key WRAPPED, because this cluster stores Secrets unencrypted in
// etcd and Velero copies them to the object store. A Secret or a backup on its
// own therefore does not let anybody impersonate a customer's phones.
func (r *Reconciler) ensureCA(ctx context.Context, label string) (*CA, error) {
	name := CASecretName(label)
	var sec corev1.Secret
	err := r.Client.Get(ctx, "v1", r.Namespace, "secrets", name, &sec)
	switch {
	case err == nil:
		certPEM := sec.Data["ca.pem"]
		sealed := sec.Data["ca.key.enc"]
		if len(certPEM) == 0 || len(sealed) == 0 {
			return nil, fmt.Errorf("takoperator: CA secret %s is incomplete; refusing to overwrite it", name)
		}
		keyPEM, uerr := UnwrapKeyMaterial(r.WrapKey, sealed)
		if uerr != nil {
			return nil, uerr
		}
		return LoadCA(certPEM, keyPEM)
	case !isNotFound(err):
		return nil, fmt.Errorf("takoperator: read CA secret: %w", err)
	}

	certPEM, keyPEM, err := NewTenantCA(label)
	if err != nil {
		return nil, err
	}
	sealed, err := WrapKeyMaterial(r.WrapKey, keyPEM)
	if err != nil {
		return nil, err
	}
	obj := corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace, Labels: objectLabels(label)},
		Data: map[string][]byte{
			"ca.pem":     certPEM,
			"ca.key.enc": sealed,
		},
	}
	if err := r.Client.Apply(ctx, "v1", r.Namespace, "secrets", name, obj); err != nil {
		return nil, fmt.Errorf("takoperator: create CA secret: %w", err)
	}
	r.Log.Info("takoperator: tenant CA created", "label", label)
	return LoadCA(certPEM, keyPEM)
}

// ensureTLS mints the instance's server certificate if it is missing or has less
// than 30 days left. Signed by the tenant CA, so the Hub verifies the instance
// against the same anchor it verifies the tenant's phones against.
func (r *Reconciler) ensureTLS(ctx context.Context, label string, ca *CA) error {
	name := TLSSecretName(label)
	var sec corev1.Secret
	err := r.Client.Get(ctx, "v1", r.Namespace, "secrets", name, &sec)
	if err == nil && len(sec.Data["tls.crt"]) > 0 && !expiringSoon(sec.Data["tls.crt"]) {
		return nil
	}
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("takoperator: read TLS secret: %w", err)
	}
	host := InstanceName(label)
	certPEM, keyPEM, err := ca.IssueServerCert(host+"."+r.Namespace+".svc",
		[]string{host, host + "." + r.Namespace, host + "." + r.Namespace + ".svc"}, 0)
	if err != nil {
		return err
	}
	obj := corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace, Labels: objectLabels(label)},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM},
	}
	if err := r.Client.Apply(ctx, "v1", r.Namespace, "secrets", name, obj); err != nil {
		return fmt.Errorf("takoperator: write TLS secret: %w", err)
	}
	return nil
}

// ensureConfig writes the values that must be PINNED for the life of the
// instance, and the nginx configuration, which may change.
//
// The pinned half is generated exactly once. Regenerating
// SECURITY_PASSWORD_SALT invalidates every password hash OpenTAKServer has
// stored, proven in spike S5, so a reconcile that "refreshed" these would lock
// every TAK user out of their own server.
func (r *Reconciler) ensureConfig(ctx context.Context, label string) error {
	name := ConfigSecretName(label)
	var sec corev1.Secret
	err := r.Client.Get(ctx, "v1", r.Namespace, "secrets", name, &sec)
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("takoperator: read config secret: %w", err)
	}
	data := map[string][]byte{}
	if sec.Data != nil {
		for k, v := range sec.Data {
			data[k] = v
		}
	}
	for _, key := range []string{"secret_key", "password_salt", "node_id", "ca_password", "admin_password"} {
		if len(data[key]) > 0 {
			continue
		}
		v, gerr := randomValue(key)
		if gerr != nil {
			return gerr
		}
		data[key] = []byte(v)
	}
	// The only mutable entry: nginx's configuration is ours and may be improved.
	data["nginx.conf"] = []byte(nginxConfig())

	obj := corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace, Labels: objectLabels(label)},
		Data:       data,
	}
	if err := r.Client.Apply(ctx, "v1", r.Namespace, "secrets", name, obj); err != nil {
		return fmt.Errorf("takoperator: write config secret: %w", err)
	}
	return nil
}

// randomValue generates each pinned value in the shape OpenTAKServer expects.
func randomValue(key string) (string, error) {
	n := 32
	if key == "node_id" {
		n = 8
	}
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("takoperator: random %s: %w", key, err)
	}
	if key == "admin_password" {
		// Base64url so it survives a JSON body and a shell without quoting.
		return base64.RawURLEncoding.EncodeToString(raw), nil
	}
	return hex.EncodeToString(raw), nil
}

// teardown is reached when a TakInstance is being deleted. It stops the
// workload, then either keeps or destroys the data depending on ONE annotation,
// and only then releases the finalizer.
//
// The default is to keep. "Stop serving this tenant" and "destroy this customer's
// history" must never be the same gesture, and a purge is the Hub's deliberate
// act at the end of the retention grace period.
func (r *Reconciler) teardown(ctx context.Context, inst *TakInstance) error {
	label := inst.Spec.Label
	purge := inst.Annotations[PurgeAnnotation] == "true"

	if err := r.Client.Delete(ctx, "apps/v1", r.Namespace, "deployments", InstanceName(label)); err != nil {
		return fmt.Errorf("delete deployment: %w", err)
	}
	if err := r.Client.Delete(ctx, "v1", r.Namespace, "services", InstanceName(label)); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}

	if purge {
		r.Log.Warn("takoperator: purging a tenant's TAK data", "label", label)
		if err := r.PurgeDatabase(ctx, label); err != nil {
			return err
		}
		// The CA last, and only on a purge: while it exists, an operator can
		// still prove which phones belonged to this tenant.
		for _, s := range []string{CASecretName(label), TLSSecretName(label), ConfigSecretName(label)} {
			if err := r.Client.Delete(ctx, "v1", r.Namespace, "secrets", s); err != nil {
				return fmt.Errorf("delete secret %s: %w", s, err)
			}
		}
	} else {
		if err := r.RetainDatabase(ctx, label); err != nil {
			return err
		}
		r.Log.Info("takoperator: instance removed, data retained", "label", label)
	}

	return r.Client.Patch(ctx, InstanceGV, r.Namespace, InstanceResource, inst.Name,
		map[string]any{"metadata": map[string]any{"finalizers": withoutFinalizer(inst.Finalizers)}})
}

func (r *Reconciler) deploymentReady(ctx context.Context, name string) (bool, error) {
	var dep struct {
		Status struct {
			ReadyReplicas int32 `json:"readyReplicas"`
		} `json:"status"`
	}
	if err := r.Client.Get(ctx, "apps/v1", r.Namespace, "deployments", name, &dep); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return dep.Status.ReadyReplicas > 0, nil
}

func (r *Reconciler) setFailed(ctx context.Context, inst *TakInstance, cause error) {
	err := r.Client.SetInstanceStatus(ctx, r.Namespace, inst.Name, TakInstanceStatus{
		Phase:              PhaseFailed,
		ObservedGeneration: inst.Generation,
		Message:            cause.Error(),
		Conditions: []metav1.Condition{{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: inst.Generation,
			LastTransitionTime: metav1.Now(),
			Reason:             PhaseFailed,
			Message:            cause.Error(),
		}},
	})
	if err != nil {
		r.Log.Warn("takoperator: could not record the failure on the object", "error", err)
	}
}

// --- small shared helpers ---

func isNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

func intstrFromInt(p int32) intstr.IntOrString { return intstr.FromInt32(p) }

func hasFinalizer(list []string) bool {
	for _, f := range list {
		if f == Finalizer {
			return true
		}
	}
	return false
}

func withoutFinalizer(list []string) []string {
	out := make([]string, 0, len(list))
	for _, f := range list {
		if f != Finalizer {
			out = append(out, f)
		}
	}
	return out
}

func boolToCondition(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}
