package takhosted

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// OTSAdmin creates and deactivates accounts inside a tenant's OpenTAKServer.
//
// # Why this exists at all
//
// A certificate is necessary and not sufficient. EudHandlerSSL reads the client
// certificate's common name and looks it up with
// `datastore.find_user(username=<CN>)`; a certificate with no matching account is
// refused at the CoT socket with "User <x> does not exist", and the certificates
// table is never consulted. So issuing a phone a certificate does not admit it --
// an OTS user row has to exist with exactly that username. That is what this does,
// and discovering it cost the phase-2 gate several hours.
//
// # How it reaches the instance
//
// OpenTAKServer's REST API binds 127.0.0.1 inside the pod. The only externally
// reachable listener is the nginx gateway on 8444, which verifies a client
// certificate against the tenant's own CA, checks the common name is
// CN=meshsat-hub, and proxies exactly three paths: /api/login, /api/user/ and
// /api/groups. Everything else is 404 by default.
//
// Every /api/user/ route is gated on @roles_accepted("administrator"), so the
// sequence is always: log in as administrator, carry the token, then act. The
// gateway had no /api/login location when it first shipped, which made the admin
// slice reachable and unusable; that is why the client distinguishes 403 (the
// Hub's Role) from 404 (the path is not proxied).
type OTSAdmin struct {
	// dial builds a client for one tenant's gateway. Injected so tests need no
	// certificates and no cluster.
	dial func(ctx context.Context, tenantID, label string) (*http.Client, string, error)

	// adminPassword returns the generated administrator password for a tenant.
	// The operator pins it in the instance's config Secret; the Hub cannot read
	// that Secret, so this is supplied by whoever can.
	adminPassword func(ctx context.Context, tenantID, label string) (string, error)

	timeout time.Duration
}

// otsUsernamePattern is what OpenTAKServer accepts.
//
// No hyphens, deliberately: upstream's UsernameValidator rejects them with HTTP
// 400, which is a confusing way to learn that "field-team-1" is not a legal TAK
// username. Checked here so the refusal names the rule instead.
var otsUsernamePattern = regexp.MustCompile(`^[a-z0-9]{3,32}$`)

// Errors worth distinguishing.
var (
	// ErrBadUsername means the name would be refused by OpenTAKServer.
	ErrBadUsername = errors.New("takhosted: a TAK username must be 3-32 lowercase letters or digits, with no hyphens")
	// ErrUserExists means the account is already there. Creating is idempotent
	// from the caller's point of view, so this is usually swallowed.
	ErrUserExists = errors.New("takhosted: the TAK user already exists")
	// ErrAdminLogin means the gateway would not accept the administrator
	// password. On a fresh instance this usually means the operator has not yet
	// replaced OpenTAKServer's default, which it does once the pod is Ready.
	ErrAdminLogin = errors.New("takhosted: could not authenticate to the instance as administrator")
)

// NewOTSAdmin builds the admin client.
func NewOTSAdmin(
	dial func(ctx context.Context, tenantID, label string) (*http.Client, string, error),
	adminPassword func(ctx context.Context, tenantID, label string) (string, error),
) *OTSAdmin {
	return &OTSAdmin{dial: dial, adminPassword: adminPassword, timeout: 20 * time.Second}
}

// GatewayDialer returns a dial function that reaches a tenant's gateway over
// mutual TLS, using the Hub identity the IdentityKeeper holds for that tenant.
//
// The identity is the same certificate the CoT proxy presents upstream: one
// CN=meshsat-hub certificate per tenant, signed by that tenant's CA. The gateway
// checks that common name, so a phone certificate from the same CA cannot be used
// here -- which is the property that keeps an admin API from being reachable with
// a customer's own credential.
func GatewayDialer(keeper *IdentityKeeper, caFor func(tenantID string) *x509.CertPool,
	hostFor func(tenantID string) string) func(context.Context, string, string) (*http.Client, string, error) {
	return func(ctx context.Context, tenantID, label string) (*http.Client, string, error) {
		identity, err := keeper.For(ctx, tenantID, label)
		if err != nil {
			return nil, "", err
		}
		roots := caFor(tenantID)
		if roots == nil {
			return nil, "", fmt.Errorf("takhosted: no CA for tenant %s", tenantID)
		}
		host := hostFor(tenantID)
		if host == "" {
			return nil, "", fmt.Errorf("takhosted: no gateway host for tenant %s", tenantID)
		}
		hc := &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					MinVersion:   tls.VersionTLS12,
					RootCAs:      roots,
					Certificates: []tls.Certificate{identity},
					// The instance's server certificate is signed by the tenant CA
					// for its in-cluster service name, so verification is real.
					ServerName: strings.SplitN(host, ":", 2)[0],
				},
				DisableKeepAlives: true,
			},
		}
		return hc, "https://" + host, nil
	}
}

// EnsureUser makes sure a TAK account exists and is active.
//
// Idempotent: an account that already exists is success, because the caller's
// intent is "this person can connect", not "I just created a row". The Hub's own
// tak_users table is the record of what should exist; this reconciles the
// instance to it.
func (a *OTSAdmin) EnsureUser(ctx context.Context, tenantID, label, username, password string) error {
	if !otsUsernamePattern.MatchString(username) {
		return fmt.Errorf("%w: %q", ErrBadUsername, username)
	}
	hc, base, err := a.dial(ctx, tenantID, label)
	if err != nil {
		return err
	}
	token, err := a.login(ctx, hc, base, tenantID, label)
	if err != nil {
		return err
	}
	body := map[string]string{
		"username":         username,
		"password":         password,
		"confirm_password": password,
	}
	raw, status, err := a.post(ctx, hc, base+"/api/user/add", token, body)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusBadRequest:
		// Upstream answers 400 for both "already exists" and a rejected name.
		// The message distinguishes them and the caller cares about the
		// difference, so it is read rather than guessed at.
		if strings.Contains(strings.ToLower(apiError(raw)), "already exists") {
			return ErrUserExists
		}
		return fmt.Errorf("takhosted: creating the TAK user: %s", apiError(raw))
	default:
		return fmt.Errorf("takhosted: creating the TAK user: status %d: %s", status, apiError(raw))
	}
}

// DeactivateUser switches an account off without deleting it.
//
// Deactivation rather than deletion, deliberately: the EUD rows, the position
// history and the audit trail all reference the user, and a tenant asking to
// remove a teammate is not asking to rewrite their map's history.
// EudHandler refuses a deactivated user at the CoT socket, and the Hub's own
// authorizer refuses them before that.
func (a *OTSAdmin) DeactivateUser(ctx context.Context, tenantID, label, username string) error {
	if !otsUsernamePattern.MatchString(username) {
		return fmt.Errorf("%w: %q", ErrBadUsername, username)
	}
	hc, base, err := a.dial(ctx, tenantID, label)
	if err != nil {
		return err
	}
	token, err := a.login(ctx, hc, base, tenantID, label)
	if err != nil {
		return err
	}
	raw, status, err := a.post(ctx, hc, base+"/api/user/deactivate", token,
		map[string]string{"username": username})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("takhosted: deactivating the TAK user: status %d: %s", status, apiError(raw))
	}
	return nil
}

// ResetUserPassword sets a new password for an account, for a customer who has
// lost theirs. OpenTAKServer refuses ':' and '@' in a password and enforces a
// minimum length, so a refusal is surfaced rather than retried.
func (a *OTSAdmin) ResetUserPassword(ctx context.Context, tenantID, label, username, newPassword string) error {
	if !otsUsernamePattern.MatchString(username) {
		return fmt.Errorf("%w: %q", ErrBadUsername, username)
	}
	if strings.ContainsAny(newPassword, ":@") {
		return errors.New("takhosted: OpenTAKServer refuses ':' and '@' in a password")
	}
	hc, base, err := a.dial(ctx, tenantID, label)
	if err != nil {
		return err
	}
	token, err := a.login(ctx, hc, base, tenantID, label)
	if err != nil {
		return err
	}
	raw, status, err := a.post(ctx, hc, base+"/api/user/password/reset", token,
		map[string]string{"username": username, "new_password": newPassword})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("takhosted: resetting the TAK password: status %d: %s", status, apiError(raw))
	}
	return nil
}

// login authenticates as the instance's administrator and returns the session
// token every /api/user/ call needs.
func (a *OTSAdmin) login(ctx context.Context, hc *http.Client, base, tenantID, label string) (string, error) {
	pw, err := a.adminPassword(ctx, tenantID, label)
	if err != nil {
		return "", fmt.Errorf("takhosted: administrator password unavailable: %w", err)
	}
	raw, status, err := a.post(ctx, hc, base+"/api/login?include_auth_token", "",
		map[string]string{"username": "administrator", "password": pw})
	if err != nil {
		return "", err
	}
	if status == http.StatusNotFound {
		// The gateway is not proxying /api/login. That was the original defect:
		// the admin slice was reachable and unusable.
		return "", fmt.Errorf("takhosted: the instance gateway does not proxy /api/login "+
			"(status %d); the admin API cannot be used without it", status)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("%w: status %d", ErrAdminLogin, status)
	}
	var out struct {
		Response struct {
			User struct {
				AuthenticationToken string `json:"authentication_token"`
			} `json:"user"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("takhosted: login returned unreadable JSON: %w", err)
	}
	if out.Response.User.AuthenticationToken == "" {
		return "", fmt.Errorf("%w: 200 with no token", ErrAdminLogin)
	}
	return out.Response.User.AuthenticationToken, nil
}

func (a *OTSAdmin) post(ctx context.Context, hc *http.Client, url, token string, body map[string]string) ([]byte, int, error) {
	enc, err := json.Marshal(body)
	if err != nil {
		return nil, 0, fmt.Errorf("takhosted: encode request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(enc))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		// flask-security's own header, not a bearer token.
		req.Header.Set("Authentication-Token", token)
	}
	resp, err := hc.Do(req) // #nosec G704 -- in-cluster service name from the instance's status
	if err != nil {
		return nil, 0, fmt.Errorf("takhosted: %s: %w", redactOTSPath(url), err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Bounded: third-party Python is answering, and an unbounded read on a
	// per-tenant loop is a memory exhaustion waiting for a bad day.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("takhosted: read response: %w", err)
	}
	return raw, resp.StatusCode, nil
}

// apiError pulls the message out of an OpenTAKServer error body, which is
// {"success": false, "error": "..."} and is written for a person.
func apiError(raw []byte) string {
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err == nil && out.Error != "" {
		return out.Error
	}
	s := strings.TrimSpace(string(raw))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// redactOTSPath keeps a username out of an error, since these URLs are built
// from one. The path itself is fixed and safe to name.
func redactOTSPath(url string) string {
	if i := strings.Index(url, "?"); i >= 0 {
		url = url[:i]
	}
	return url
}
