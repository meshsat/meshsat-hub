package takoperator

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

// ErrNotFound is returned for a 404, so callers can tell "absent" from "broken".
var ErrNotFound = errors.New("takoperator: object not found")

// ErrForbidden is returned for a 403. Worth its own error because it almost
// always means the operator's Role is missing a rule rather than that anything
// is wrong with the object.
var ErrForbidden = errors.New("takoperator: forbidden (check the operator's Role)")

// fieldManager identifies this operator's writes in managedFields, so a
// server-side apply replaces only the fields it owns and leaves anybody else's
// alone.
const fieldManager = "tak-operator"

// Client is a minimal Kubernetes API client that speaks plain JSON.
//
// The same reasoning as internal/bridge/casecret.go: this operator touches four
// kinds, two of which are custom, and linking the typed clients plus the
// generated deep-copy code for them would cost tens of megabytes and a
// code-generation step to save a few dozen lines here.
//
// Writes are server-side applies, which makes create and update the same call
// and therefore makes every reconcile idempotent without a get-then-decide
// dance.
type Client struct {
	http *http.Client
	base string
}

// NewInClusterClient builds a client from the pod's ServiceAccount.
func NewInClusterClient() (*Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("takoperator: in-cluster config: %w", err)
	}
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("takoperator: api client: %w", err)
	}
	return NewClientWith(hc, cfg.Host), nil
}

// NewClientWith is the constructor tests use: client must already carry the
// credentials, base is the API server URL.
func NewClientWith(hc *http.Client, base string) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{http: hc, base: strings.TrimRight(base, "/")}
}

// Namespace returns the namespace the operator runs in, which is also the
// namespace it manages.
func Namespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return DefaultNamespace
}

// groupVersionPath turns "apps/v1" into "/apis/apps/v1" and the core group's
// "v1" into "/api/v1".
func groupVersionPath(gv string) string {
	if !strings.Contains(gv, "/") {
		return "/api/" + url.PathEscape(gv)
	}
	parts := strings.SplitN(gv, "/", 2)
	return "/apis/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
}

func (c *Client) path(gv, namespace, resource, name, subresource string) string {
	p := c.base + groupVersionPath(gv)
	if namespace != "" {
		p += "/namespaces/" + url.PathEscape(namespace)
	}
	p += "/" + url.PathEscape(resource)
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	if subresource != "" {
		p += "/" + url.PathEscape(subresource)
	}
	return p
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
		return fmt.Errorf("takoperator: %s %s: %w", method, redactPath(path), err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	_ = resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, redactPath(path))
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s %s", ErrForbidden, method, redactPath(path))
	case resp.StatusCode >= 300:
		return fmt.Errorf("takoperator: %s %s: status %d: %s",
			method, redactPath(path), resp.StatusCode, apiMessage(raw))
	}
	if readErr != nil {
		return fmt.Errorf("takoperator: read response: %w", readErr)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("takoperator: decode response: %w", err)
	}
	return nil
}

// redactPath keeps object names out of logs where they could identify a tenant.
// Labels are opaque, so they are safe; this trims query strings, which carry the
// field manager and force flags and add noise.
func redactPath(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// apiMessage pulls the human-readable part out of a Kubernetes Status object, so
// an error says "Deployment.apps is invalid: ..." rather than a wall of JSON.
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

// Get fetches one object. A missing object is ErrNotFound.
func (c *Client) Get(ctx context.Context, gv, namespace, resource, name string, out any) error {
	return c.do(ctx, http.MethodGet, c.path(gv, namespace, resource, name, ""), "", nil, out)
}

// List fetches a whole collection. No pagination: the collections this operator
// reads are one object per tenant, and the tier holds tens of tenants.
func (c *Client) List(ctx context.Context, gv, namespace, resource string, out any) error {
	return c.do(ctx, http.MethodGet, c.path(gv, namespace, resource, "", ""), "", nil, out)
}

// Apply creates or updates an object with a server-side apply. obj must carry
// apiVersion, kind and metadata.name, because the server needs them to resolve
// the patch.
//
// force is set: the operator owns these objects, so if a human or another
// controller has taken a field the operator manages, the operator takes it back
// rather than wedging with a conflict nobody is watching.
func (c *Client) Apply(ctx context.Context, gv, namespace, resource, name string, obj any) error {
	body, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("takoperator: encode %s: %w", resource, err)
	}
	p := c.path(gv, namespace, resource, name, "") +
		"?fieldManager=" + fieldManager + "&force=true"
	// JSON is valid YAML, so an apply-patch may carry a JSON body.
	return c.do(ctx, http.MethodPatch, p, "application/apply-patch+yaml", body, nil)
}

// PatchStatus merges a status document into an object's status subresource.
// Merge-patch rather than apply, because a status is written by one writer and
// the conditions list should replace rather than accumulate.
func (c *Client) PatchStatus(ctx context.Context, gv, namespace, resource, name string, status any) error {
	body, err := json.Marshal(map[string]any{"status": status})
	if err != nil {
		return fmt.Errorf("takoperator: encode status: %w", err)
	}
	p := c.path(gv, namespace, resource, name, "status") + "?fieldManager=" + fieldManager
	return c.do(ctx, http.MethodPatch, p, "application/merge-patch+json", body, nil)
}

// Patch applies a merge patch to an object, used for metadata such as removing a
// finalizer.
func (c *Client) Patch(ctx context.Context, gv, namespace, resource, name string, patch any) error {
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("takoperator: encode patch: %w", err)
	}
	p := c.path(gv, namespace, resource, name, "") + "?fieldManager=" + fieldManager
	return c.do(ctx, http.MethodPatch, p, "application/merge-patch+json", body, nil)
}

// Delete removes an object. A missing object is not an error: the desired state
// is "gone", and it is gone.
func (c *Client) Delete(ctx context.Context, gv, namespace, resource, name string) error {
	err := c.do(ctx, http.MethodDelete, c.path(gv, namespace, resource, name, ""), "", nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// --- typed conveniences over the above, so the reconcilers read cleanly ---

// InstanceGV and CertReqGV are the custom API's group/version string.
var (
	InstanceGV = Group + "/" + Version
	CNPGGV     = CNPGGroup + "/" + CNPGVersion
)

// ListInstances returns every TakInstance in the namespace.
func (c *Client) ListInstances(ctx context.Context, namespace string) ([]TakInstance, error) {
	var list TakInstanceList
	if err := c.List(ctx, InstanceGV, namespace, InstanceResource, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// GetInstance returns one TakInstance.
func (c *Client) GetInstance(ctx context.Context, namespace, name string) (*TakInstance, error) {
	var inst TakInstance
	if err := c.Get(ctx, InstanceGV, namespace, InstanceResource, name, &inst); err != nil {
		return nil, err
	}
	return &inst, nil
}

// SetInstanceStatus writes a TakInstance's status.
func (c *Client) SetInstanceStatus(ctx context.Context, namespace, name string, st TakInstanceStatus) error {
	return c.PatchStatus(ctx, InstanceGV, namespace, InstanceResource, name, st)
}

// ListCertRequests returns every TakCertificateRequest in the namespace.
func (c *Client) ListCertRequests(ctx context.Context, namespace string) ([]TakCertificateRequest, error) {
	var list TakCertificateRequestList
	if err := c.List(ctx, InstanceGV, namespace, CertReqResource, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// SetCertRequestStatus writes a TakCertificateRequest's status.
func (c *Client) SetCertRequestStatus(ctx context.Context, namespace, name string, st TakCertificateRequestStatus) error {
	return c.PatchStatus(ctx, InstanceGV, namespace, CertReqResource, name, st)
}

// ListUserRequests returns every TakUserRequest in the namespace.
func (c *Client) ListUserRequests(ctx context.Context, namespace string) ([]TakUserRequest, error) {
	var list TakUserRequestList
	if err := c.List(ctx, InstanceGV, namespace, UserReqResource, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// SetUserRequestStatus writes a TakUserRequest's status.
func (c *Client) SetUserRequestStatus(ctx context.Context, namespace, name string, st TakUserRequestStatus) error {
	return c.PatchStatus(ctx, InstanceGV, namespace, UserReqResource, name, st)
}
