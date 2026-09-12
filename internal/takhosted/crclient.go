// Package takhosted is the Hub's side of hosted per-tenant TAK (MESHSAT-1037).
//
// It turns the operator's TakInstance objects into a takfront directory, answers
// "is this phone still allowed" from the tak_users table, and asks the operator
// for the certificates the Hub needs to reach each tenant's OpenTAKServer.
//
// # Why this package does not import internal/takoperator
//
// The two packages speak about the same custom resources, and reusing the
// operator's client and structs would save perhaps eighty lines. It is not worth
// it, and the reason is the whole point of having an operator at all.
//
// internal/takoperator exports NewTenantCA, LoadCA, ParseWrapKey and
// UnwrapKeyMaterial. Its own package doc says why it exists: "the Hub is
// internet-facing, and whoever holds a tenant's CA key can impersonate any phone
// in that tenant. So the key lives here, in a process with no public listener,
// and the Hub gets certificates by asking." Importing that package would give the
// internet-facing binary compile-time reach over exactly that material.
//
// Nothing would be exploitable today: the Hub has no wrap key, and its Role
// grants no access to the Secrets holding wrapped CA keys. But that is a runtime
// accident, not a boundary. A boundary is something a reviewer can check, so the
// import is absent and boundary_test.go fails if it returns.
//
// The cost is four small structs and one HTTP helper duplicated from the
// operator. They are pinned to the CRDs by the API server, which rejects a
// malformed spec, and by the release gate.
package takhosted

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/rest"
)

// API coordinates of the custom resources. Duplicated from the operator rather
// than imported; see the package doc.
const (
	crGroup          = "tak.meshsat.net"
	crVersion        = "v1alpha1"
	instanceResource = "takinstances"
	certReqResource  = "takcertificaterequests"
	// userReqResource asks the operator for an OpenTAKServer account. The Hub
	// cannot create one itself: every /api/user/ route needs an administrator
	// session, that password lives in the instance's config Secret, and the Hub's
	// Role grants no Secrets in this namespace -- nor should it, since the same
	// Secret carries secret_key and password_salt and RBAC cannot grant one key.
	userReqResource = "takuserrequests"

	// defaultNamespace holds the custom resources and the instances. The Hub
	// runs in meshsat-hub and reaches across to it, which is why its Role for
	// these resources is a separate Role in that namespace.
	defaultNamespace = "meshsat-tak"

	// fieldManager identifies the Hub's writes in managedFields, so a
	// server-side apply touches only the fields the Hub owns.
	fieldManager = "meshsat-hub"
)

// Phases and purposes the Hub reads or asks for.
const (
	PhaseReady  = "Ready"
	CertIssued  = "Issued"
	CertDenied  = "Denied"
	PurposeHub  = "hub"
	PurposeEUD  = "eud"
	HubIdentity = "meshsat-hub"
)

// Account actions and phases, mirroring the operator's own constants. Duplicated
// for the same reason as everything else here: this package may not import the
// operator, which holds every tenant's CA key.
const (
	// ActionEnsure means the account should exist and be able to connect.
	ActionEnsure = "Ensure"
	// ActionDeactivate switches it off without deleting it, so the EUD rows, the
	// position history and the audit trail that reference the user survive.
	ActionDeactivate = "Deactivate"

	UserPending = "Pending"
	UserApplied = "Applied"
	UserDenied  = "Denied"
)

// Errors a caller may want to distinguish.
var (
	// ErrNotFound is a 404: the object is absent rather than unreachable.
	ErrNotFound = errors.New("takhosted: object not found")
	// ErrForbidden is a 403, which on this path almost always means the Hub's
	// Role in meshsat-tak is missing a rule rather than that anything is wrong
	// with the request. It is worth its own error for that reason: the Role is
	// new, cross-namespace, and the first thing to suspect.
	ErrForbidden = errors.New("takhosted: forbidden (check the Hub's Role in meshsat-tak)")
)

// TakInstance is the Hub's view of one tenant's hosted OpenTAKServer. Only the
// fields the Hub reads or sets are modelled; the API server holds the rest.
type TakInstance struct {
	APIVersion string       `json:"apiVersion,omitempty"`
	Kind       string       `json:"kind,omitempty"`
	Metadata   objectMeta   `json:"metadata,omitempty"`
	Spec       InstanceSpec `json:"spec"`
	Status     InstanceStat `json:"status,omitempty"`
}

// InstanceSpec is everything the Hub may ask for. Deliberately tiny: no image,
// no resources, no command. What a tenant gets is the operator's decision.
type InstanceSpec struct {
	TenantID string `json:"tenantID"`
	Label    string `json:"label"`
	State    string `json:"state,omitempty"`
	Files    bool   `json:"files,omitempty"`
}

// InstanceStat is written only by the operator; the Hub reads it.
type InstanceStat struct {
	Phase     string `json:"phase,omitempty"`
	Host      string `json:"host,omitempty"`
	CACertPEM string `json:"caCertPEM,omitempty"`
	Message   string `json:"message,omitempty"`
}

// TakCertificateRequest asks the operator to sign a CSR as a named user. The
// CSR's own subject is discarded by the issuer, which is what stops a tenant
// asking for a certificate naming somebody else.
type TakCertificateRequest struct {
	APIVersion string      `json:"apiVersion,omitempty"`
	Kind       string      `json:"kind,omitempty"`
	Metadata   objectMeta  `json:"metadata,omitempty"`
	Spec       CertReqSpec `json:"spec"`
	Status     CertReqStat `json:"status,omitempty"`
}

// CertReqSpec names the instance, the purpose and the user.
type CertReqSpec struct {
	Label    string `json:"label"`
	Purpose  string `json:"purpose"`
	Username string `json:"username"`
	CSRPEM   string `json:"csrPEM"`
}

// CertReqStat is written only by the operator.
type CertReqStat struct {
	Phase     string `json:"phase,omitempty"`
	CertPEM   string `json:"certPEM,omitempty"`
	CACertPEM string `json:"caCertPEM,omitempty"`
	Serial    string `json:"serial,omitempty"`
	Message   string `json:"message,omitempty"`
}

// objectMeta is the sliver of metadata the Hub sets or reads.
type objectMeta struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	// Generation is bumped by the API server on every spec change, and is what
	// makes a status trustworthy: a TakUserRequest whose action was patched from
	// Ensure to Deactivate keeps its old "Applied" status until the operator acts,
	// so a reader that ignored this would report a teammate switched off while
	// they were still able to connect. Compare it with status.observedGeneration.
	Generation  int64             `json:"generation,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type instanceList struct {
	Items []TakInstance `json:"items"`
}

// Client is a minimal Kubernetes API client speaking plain JSON.
//
// Same reasoning as internal/bridge/casecret.go, which this mirrors: the Hub
// touches two custom kinds, and linking the typed clients plus generated
// deep-copy code would cost tens of megabytes to save a few dozen lines.
//
// Writes are server-side applies, so create and update are one idempotent call.
type Client struct {
	http      *http.Client
	base      string
	namespace string
}

// NewInClusterClient builds a client from the pod's ServiceAccount.
func NewInClusterClient(namespace string) (*Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("takhosted: in-cluster config: %w", err)
	}
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("takhosted: api client: %w", err)
	}
	return NewClientWith(hc, cfg.Host, namespace), nil
}

// NewClientWith is the constructor tests use: hc must already carry credentials,
// base is the API server URL.
func NewClientWith(hc *http.Client, base, namespace string) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	if namespace == "" {
		namespace = TakNamespace()
	}
	return &Client{http: hc, base: strings.TrimRight(base, "/"), namespace: namespace}
}

// TakNamespace is where the custom resources live. Overridable for a test
// cluster, but never the Hub's own namespace: the resources are the operator's.
func TakNamespace() string {
	if ns := os.Getenv("HUB_TAK_NAMESPACE"); ns != "" {
		return ns
	}
	return defaultNamespace
}

// Namespace reports the namespace this client addresses.
func (c *Client) Namespace() string { return c.namespace }

func (c *Client) collectionPath(resource string) string {
	return c.base + "/apis/" + crGroup + "/" + crVersion +
		"/namespaces/" + url.PathEscape(c.namespace) + "/" + resource
}

func (c *Client) objectPath(resource, name string) string {
	return c.collectionPath(resource) + "/" + url.PathEscape(name)
}

// ListInstances returns every tenant's instance.
//
// Not tenant-scoped, and that is deliberate: the Hub serves one TLS listener for
// every tenant, so it has to resolve any phone's certificate issuer and
// therefore needs the whole set.
func (c *Client) ListInstances(ctx context.Context) ([]TakInstance, error) {
	var out instanceList
	if err := c.do(ctx, http.MethodGet, c.collectionPath(instanceResource), "", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// GetInstance fetches one. A missing instance is ErrNotFound.
func (c *Client) GetInstance(ctx context.Context, name string) (*TakInstance, error) {
	var inst TakInstance
	if err := c.do(ctx, http.MethodGet, c.objectPath(instanceResource, name), "", nil, &inst); err != nil {
		return nil, err
	}
	return &inst, nil
}

// ApplyInstance creates or updates an instance. The Hub owns the spec; the
// operator owns the status, and a server-side apply from this field manager
// leaves the operator's fields alone.
func (c *Client) ApplyInstance(ctx context.Context, inst TakInstance) error {
	inst.APIVersion = crGroup + "/" + crVersion
	inst.Kind = "TakInstance"
	if inst.Metadata.Name == "" {
		return errors.New("takhosted: instance needs a name")
	}
	inst.Metadata.Namespace = c.namespace
	body, err := json.Marshal(inst)
	if err != nil {
		return fmt.Errorf("takhosted: encode instance: %w", err)
	}
	path := c.objectPath(instanceResource, inst.Metadata.Name) +
		"?fieldManager=" + fieldManager + "&force=true"
	return c.do(ctx, http.MethodPatch, path, "application/apply-patch+yaml", body, nil)
}

// DeleteInstance removes an instance. Whether the tenant's DATA goes with it is
// decided by the purge annotation the Hub sets before deleting, not by this call:
// "stop serving this tenant" and "destroy this customer's history" must never be
// the same gesture.
func (c *Client) DeleteInstance(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, c.objectPath(instanceResource, name), "", nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// AnnotateInstance merge-patches annotations, which is how the purge marker is
// set before a delete.
func (c *Client) AnnotateInstance(ctx context.Context, name string, annotations map[string]string) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": annotations},
	})
	if err != nil {
		return fmt.Errorf("takhosted: encode annotation patch: %w", err)
	}
	return c.do(ctx, http.MethodPatch, c.objectPath(instanceResource, name),
		"application/merge-patch+json", patch, nil)
}

// CreateCertRequest submits a CSR for signing. The name is chosen by the caller
// so a retry lands on the same object rather than queueing a second signature.
func (c *Client) CreateCertRequest(ctx context.Context, req TakCertificateRequest) error {
	req.APIVersion = crGroup + "/" + crVersion
	req.Kind = "TakCertificateRequest"
	if req.Metadata.Name == "" {
		return errors.New("takhosted: certificate request needs a name")
	}
	req.Metadata.Namespace = c.namespace
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("takhosted: encode certificate request: %w", err)
	}
	path := c.objectPath(certReqResource, req.Metadata.Name) +
		"?fieldManager=" + fieldManager + "&force=true"
	return c.do(ctx, http.MethodPatch, path, "application/apply-patch+yaml", body, nil)
}

// GetCertRequest reads one back, which is how the Hub collects the issued
// certificate. A missing request is ErrNotFound.
func (c *Client) GetCertRequest(ctx context.Context, name string) (*TakCertificateRequest, error) {
	var req TakCertificateRequest
	if err := c.do(ctx, http.MethodGet, c.objectPath(certReqResource, name), "", nil, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// DeleteCertRequest removes a request once its certificate has been collected.
// Leaving them would accumulate one object per enrollment, each carrying a
// certificate that is public but still identifies a user.
func (c *Client) DeleteCertRequest(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, c.objectPath(certReqResource, name), "", nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// TakUserRequest asks the operator to create or deactivate one OpenTAKServer
// account. The minimal shape again, for the same reason as the others.
//
// Note what is absent: a password. The operator generates one and the Hub never
// learns it. A TAK client authenticates with its certificate, and the account's
// password exists only because OpenTAKServer requires a user row to have one.
type TakUserRequest struct {
	APIVersion string      `json:"apiVersion,omitempty"`
	Kind       string      `json:"kind,omitempty"`
	Metadata   objectMeta  `json:"metadata,omitempty"`
	Spec       UserReqSpec `json:"spec"`
	Status     UserReqStat `json:"status,omitempty"`
}

// UserReqSpec is desired state: label and username are the account's identity and
// the CRD holds them immutable, while action may be patched.
type UserReqSpec struct {
	Label    string `json:"label"`
	Username string `json:"username"`
	Action   string `json:"action"`
}

// UserReqStat is written only by the operator. The Hub's Role carries no status
// verb on this kind, so an attempt to write it would be a 403.
type UserReqStat struct {
	Phase              string `json:"phase,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	Message            string `json:"message,omitempty"`
}

// userRequestName is the object name for one account request.
//
// Deterministic, so asking twice lands on the same object rather than queueing a
// second one, and built from the opaque label rather than the tenant id because
// the name is visible in the namespace and the label leaks nothing about who the
// customer is -- the same rule as certRequestName.
//
// DNS-safe by construction: the label is ten lowercase alphanumerics and the
// username three to thirty-two of the same, so the result carries only those and
// two hyphens, and is at most 48 characters.
func userRequestName(label, username string) string {
	return "user-" + label + "-" + username
}

// CreateUserRequest asks for an account, and is also how the Hub changes one:
// server-side apply makes create and update a single idempotent call, which is
// what a declarative object wants. Switching an account off is this call with
// action Deactivate.
func (c *Client) CreateUserRequest(ctx context.Context, req TakUserRequest) error {
	req.APIVersion = crGroup + "/" + crVersion
	req.Kind = "TakUserRequest"
	if req.Metadata.Name == "" {
		return errors.New("takhosted: user request needs a name")
	}
	req.Metadata.Namespace = c.namespace
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("takhosted: encode user request: %w", err)
	}
	path := c.objectPath(userReqResource, req.Metadata.Name) +
		"?fieldManager=" + fieldManager + "&force=true"
	return c.do(ctx, http.MethodPatch, path, "application/apply-patch+yaml", body, nil)
}

// GetUserRequest reads one back, which is how the Hub learns whether the account
// exists yet. A missing request is ErrNotFound.
func (c *Client) GetUserRequest(ctx context.Context, name string) (*TakUserRequest, error) {
	var req TakUserRequest
	if err := c.do(ctx, http.MethodGet, c.objectPath(userReqResource, name), "", nil, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// DeleteUserRequest removes a request object. Deleting it does NOT remove the
// account from the instance -- the operator acts on a spec, it does not own the
// account's lifetime -- so this is cleanup of a finished request, and removing a
// teammate means applying action Deactivate instead.
func (c *Client) DeleteUserRequest(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, c.objectPath(userReqResource, name), "", nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// do performs a request and decodes a JSON body into out when out is non-nil.
func (c *Client) do(ctx context.Context, method, path, contentType string, body []byte, out any) error {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req) // #nosec G704 -- in-cluster API server address from the ServiceAccount config
	if err != nil {
		return fmt.Errorf("takhosted: %s %s: %w", method, redactPath(path), err)
	}
	// Bounded: the API server is not hostile, but a reconcile loop that can be
	// made to read without limit is a memory exhaustion waiting for a bad day.
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	_ = resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, redactPath(path))
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s %s", ErrForbidden, method, redactPath(path))
	case resp.StatusCode >= 300:
		return fmt.Errorf("takhosted: %s %s: status %d: %s",
			method, redactPath(path), resp.StatusCode, apiMessage(raw))
	}
	if readErr != nil {
		return fmt.Errorf("takhosted: read response: %w", readErr)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("takhosted: decode response: %w", err)
	}
	return nil
}

// redactPath keeps object names out of logs, because an instance name carries a
// tenant id and a certificate request name carries a username. Labels are opaque
// by design and safe; the query string only holds the field manager and force
// flag, so it is trimmed as noise.
func redactPath(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	// Keep the collection, drop the object name: ".../takinstances/<name>".
	for _, res := range []string{instanceResource, certReqResource} {
		marker := "/" + res + "/"
		if i := strings.Index(p, marker); i >= 0 {
			return p[:i+len(marker)] + "<redacted>"
		}
	}
	return p
}

// apiMessage pulls the human-readable part out of a Kubernetes Status object, so
// an error reads as a sentence rather than a wall of JSON.
func apiMessage(raw []byte) string {
	var st struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &st); err == nil && st.Message != "" {
		return st.Message
	}
	if len(raw) > 400 {
		raw = raw[:400]
	}
	return string(raw)
}

// IsNotFound reports whether err is a 404 from this client.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsForbidden reports whether err is a 403 from this client, which means the
// Hub's Role is missing a rule.
func IsForbidden(err error) bool { return errors.Is(err, ErrForbidden) }
