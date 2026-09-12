// Package takoperator reconciles hosted per-tenant OpenTAKServer instances.
//
// It is a small controller with one job: turn a TakInstance into a running
// OpenTAKServer with its own CA, its own database and its own role, and turn a
// TakCertificateRequest into a certificate whose subject the requester does not
// choose.
//
// It talks to the API server over plain JSON with the in-cluster REST config,
// the same way internal/bridge/casecret.go does, rather than through
// controller-runtime: this repo already carries k8s.io/client-go and
// apimachinery, and the typed clients and their generated deep-copy machinery
// would add tens of megabytes and a code-generation step for what amounts to
// four object kinds. The structs below are therefore hand-written and the
// reconcile loop is a ticker, not an informer.
//
// Why the operator exists at all, rather than the Hub doing this itself: the Hub
// is internet-facing, and whoever holds a tenant's CA key can impersonate any
// phone in that tenant. So the key lives here, in a process with no public
// listener, and the Hub gets certificates by asking.
package takoperator

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The API this operator owns.
const (
	// Group and Version of both custom resources.
	Group   = "tak.meshsat.net"
	Version = "v1alpha1"

	// Resource names, as they appear in API paths.
	InstanceResource = "takinstances"
	CertReqResource  = "takcertificaterequests"

	// Namespace holding both the custom resources and the instances.
	DefaultNamespace = "meshsat-tak"
	// DBNamespace holds the CNPG cluster and the per-tenant Database and
	// DatabaseRole objects.
	DefaultDBNamespace = "meshsat-tak-db"
	// DBCluster is the CNPG cluster every tenant database belongs to.
	DefaultDBCluster = "meshsat-tak-main"
)

// Instance states a TakInstance spec may ask for.
const (
	StateRunning    = "Running"
	StateSuspended  = "Suspended"
	StateHibernated = "Hibernated"
)

// Phases a TakInstance status may report.
const (
	PhasePending      = "Pending"
	PhaseProvisioning = "Provisioning"
	PhaseReady        = "Ready"
	PhaseSuspended    = "Suspended"
	PhaseHibernated   = "Hibernated"
	PhaseFailed       = "Failed"
)

// Certificate request purposes and phases.
const (
	PurposeEUD = "eud"
	PurposeHub = "hub"

	CertPending = "Pending"
	CertIssued  = "Issued"
	CertDenied  = "Denied"

	// EUDValidDays is a phone's certificate life. Long enough that a field team
	// is not re-enrolling mid-deployment, short enough that a lost phone's
	// certificate expires on its own even if nobody tells us.
	EUDValidDays = 90
	// HubValidDays is the Hub's own upstream identity. Short because renewal is
	// automatic and costs nothing.
	HubValidDays = 7
)

// PurgeAnnotation, when present on a TakInstance being deleted, is what allows
// the operator to drop the tenant's database and role. Without it a delete
// tears down the workload and RETAINS the data, because "stop serving this" and
// "destroy this customer's history" must not be the same gesture.
const PurgeAnnotation = "tak.meshsat.net/purge-data"

// TakInstance is one customer's hosted OpenTAKServer.
type TakInstance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TakInstanceSpec   `json:"spec"`
	Status TakInstanceStatus `json:"status,omitempty"`
}

// TakInstanceSpec is everything the Hub may ask for. Deliberately tiny: no
// image, no hostname, no resources, no command.
type TakInstanceSpec struct {
	TenantID string `json:"tenantID"`
	Label    string `json:"label"`
	State    string `json:"state,omitempty"`
	Files    bool   `json:"files,omitempty"`
}

// TakInstanceStatus is written only by the operator.
type TakInstanceStatus struct {
	Phase              string             `json:"phase,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Host               string             `json:"host,omitempty"`
	CACertPEM          string             `json:"caCertPEM,omitempty"`
	Message            string             `json:"message,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// TakInstanceList is a list response.
type TakInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	Metadata        metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TakInstance   `json:"items"`
}

// TakCertificateRequest asks the operator to sign a CSR as a named user.
type TakCertificateRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TakCertificateRequestSpec   `json:"spec"`
	Status TakCertificateRequestStatus `json:"status,omitempty"`
}

// TakCertificateRequestSpec names the instance, the purpose and the user. The
// CSR's own subject is discarded by the issuer.
type TakCertificateRequestSpec struct {
	Label    string `json:"label"`
	Purpose  string `json:"purpose"`
	Username string `json:"username"`
	CSRPEM   string `json:"csrPEM"`
}

// TakCertificateRequestStatus is written only by the operator.
type TakCertificateRequestStatus struct {
	Phase              string       `json:"phase,omitempty"`
	ObservedGeneration int64        `json:"observedGeneration,omitempty"`
	CertPEM            string       `json:"certPEM,omitempty"`
	CACertPEM          string       `json:"caCertPEM,omitempty"`
	Serial             string       `json:"serial,omitempty"`
	NotAfter           *metav1.Time `json:"notAfter,omitempty"`
	Message            string       `json:"message,omitempty"`
}

// TakCertificateRequestList is a list response.
type TakCertificateRequestList struct {
	metav1.TypeMeta `json:",inline"`
	Metadata        metav1.ListMeta         `json:"metadata,omitempty"`
	Items           []TakCertificateRequest `json:"items"`
}

// --- the CNPG objects the operator creates, as much of them as it sets ---
//
// CloudNativePG 1.30 serves both `databases` and `databaseroles` in
// postgresql.cnpg.io/v1 (verified on the cluster, not assumed). That is why the
// operator never patches the Cluster object: per-tenant state is its own object,
// so Argo's selfHeal has nothing to fight over.

// CNPG API coordinates.
const (
	CNPGGroup        = "postgresql.cnpg.io"
	CNPGVersion      = "v1"
	DatabaseResource = "databases"
	RoleResource     = "databaseroles"

	// ReclaimRetain leaves the database or role in place when the Kubernetes
	// object goes away. This is the default the operator writes, so that losing
	// an object cannot lose a customer's history.
	ReclaimRetain = "retain"
	// ReclaimDelete is written only during an annotated purge.
	ReclaimDelete = "delete"

	// RoleConnectionLimit caps one tenant's connections to the shared cluster.
	// The cluster allows 200, so this is about one noisy tenant not starving the
	// others rather than about capacity.
	RoleConnectionLimit = 15
)

// ClusterRef points a Database or DatabaseRole at its cluster.
type ClusterRef struct {
	Name string `json:"name"`
}

// SecretRef names a Secret holding a role password.
type SecretRef struct {
	Name string `json:"name"`
}

// CNPGDatabase is a postgresql.cnpg.io/v1 Database.
type CNPGDatabase struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CNPGDatabaseSpec `json:"spec"`
}

// CNPGDatabaseSpec is the subset the operator sets.
type CNPGDatabaseSpec struct {
	Cluster          ClusterRef `json:"cluster"`
	Name             string     `json:"name"`
	Owner            string     `json:"owner"`
	Ensure           string     `json:"ensure,omitempty"`
	AllowConnections *bool      `json:"allowConnections,omitempty"`
	ConnectionLimit  *int       `json:"connectionLimit,omitempty"`
	ReclaimPolicy    string     `json:"databaseReclaimPolicy,omitempty"`
}

// CNPGDatabaseRole is a postgresql.cnpg.io/v1 DatabaseRole.
type CNPGDatabaseRole struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CNPGDatabaseRoleSpec `json:"spec"`
}

// CNPGDatabaseRoleSpec is the subset the operator sets. Note what is absent:
// superuser, createdb, createrole, bypassrls and replication are all left unset,
// so a tenant role can do nothing but log in and use its own database.
type CNPGDatabaseRoleSpec struct {
	Cluster         ClusterRef `json:"cluster"`
	Name            string     `json:"name"`
	Ensure          string     `json:"ensure,omitempty"`
	Login           *bool      `json:"login,omitempty"`
	ConnectionLimit *int       `json:"connectionLimit,omitempty"`
	PasswordSecret  *SecretRef `json:"passwordSecret,omitempty"`
	Comment         string     `json:"comment,omitempty"`
	ReclaimPolicy   string     `json:"databaseRoleReclaimPolicy,omitempty"`
}

// Names of the per-instance objects, derived from the label so that two tenants
// can never collide and so that a name leaks nothing about the customer.
func InstanceName(label string) string { return "tak-" + label }

// DatabaseName is also the ROLE name, because the cluster's pg_hba is
// `host sameuser all all scram-sha-256`: a role may reach only the database
// whose name matches its own. Keeping them equal is what makes that rule an
// isolation boundary rather than a formality.
func DatabaseName(label string) string { return "tak_" + label }

// CASecretName holds the tenant CA certificate and its WRAPPED key.
func CASecretName(label string) string { return "tak-" + label + "-ca" }

// TLSSecretName holds the instance's server certificate and key.
func TLSSecretName(label string) string { return "tak-" + label + "-tls" }

// ConfigSecretName holds the pinned OpenTAKServer values: SECRET_KEY, the
// password salt, the node id and the CA password. Pinned once and never
// regenerated, because changing the salt invalidates every stored password hash.
func ConfigSecretName(label string) string { return "tak-" + label + "-config" }

// DBSecretName holds the tenant database role's password.
func DBSecretName(label string) string { return "tak-" + label + "-db" }
