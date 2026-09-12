package takoperator

// Taking a tenant's OpenTAKServer off its default administrator password.
//
// OpenTAKServer creates an `administrator` account on first start with the
// literal password "password" — app.py says so in as many words:
//
//	logger.info("Creating administrator account. The password is 'password'")
//
// It can only be changed through the REST API, which needs the schema migrated
// and the API listening. Neither is true while the init container runs, which is
// why the first attempt at this — writing the generated password to a file in the
// data folder — did nothing at all: no code in 1.7.13 reads that path. The file
// was inert, and a commit message claimed on the strength of it that the
// documented default "never works" on a hosted instance. It worked. Probed on the
// live gate instance, HTTP 200 with a session token.
//
// So the change belongs here: after the Deployment reports a ready replica, over
// the same mutual-TLS gateway the Hub uses, with the operator minting itself a
// short-lived CN=meshsat-hub certificate from the tenant's own CA.
//
// On exposure, stated precisely rather than dramatically: the API binds
// 127.0.0.1 inside the pod, the only listener reachable from outside is nginx on
// 8444 with `ssl_verify_client on` against the tenant CA, and that config is
// default-deny with a handful of named paths. So the default credential was
// never reachable from the internet — it was reachable from inside the pod. This
// is defence in depth, and the kind a hosted service has no excuse to skip.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	// HubIdentityCN is the common name the Hub — and the operator, acting through
	// the same door — presents to an instance's admin gateway. The nginx there
	// matches this exact subject component, because the tenant CA also signs
	// every phone in the tenant and a phone certificate must never be usable as
	// an admin credential.
	HubIdentityCN = "meshsat-hub"

	// otsAdminUsername is the account OpenTAKServer bootstraps for itself.
	otsAdminUsername = "administrator"

	// defaultOTSAdminPassword is what OpenTAKServer sets that account to. This is
	// not a secret of ours — it is published in upstream's own log line — and it
	// is here so the operator can ask whether it still works.
	defaultOTSAdminPassword = "password"

	// adminHTTPTimeout bounds one call. The whole bootstrap is three or four of
	// them, and it runs on a reconcile tick, so there is no reason to wait long:
	// a slow instance is retried in seconds.
	adminHTTPTimeout = 15 * time.Second
)

// ErrAdminPasswordStillDefault means the reset returned success and the default
// password still authenticates. That is the one outcome that must never be
// logged and forgotten: it looks like the job was done.
var ErrAdminPasswordStillDefault = errors.New(
	"takoperator: administrator still accepts the default password after a successful reset")

// bootstrapAdmin changes the instance's administrator password to the value the
// operator pinned in the config Secret, and proves the default no longer works.
//
// Idempotent by construction: it begins by asking whether the default still
// authenticates, so a second pass over an already-bootstrapped instance makes one
// request and returns. That is cheaper than a status flag and cannot go stale —
// a flag says what we did once, this asks the instance what is true now.
func (r *Reconciler) bootstrapAdmin(ctx context.Context, label string, ca *CA) error {
	want, err := r.adminPassword(ctx, label)
	if err != nil {
		return err
	}
	// OpenTAKServer's own reset endpoint refuses these two characters, and a
	// minimum length. Catching it here names the cause; leaving it to the API
	// produces a 400 whose body nobody reads.
	if strings.ContainsAny(want, ":@") {
		return errors.New("takoperator: the generated administrator password contains ':' or '@', " +
			"which OpenTAKServer's reset endpoint refuses; regenerate it")
	}
	cl, err := instanceAdminClient(ca, label, r.Namespace)
	if err != nil {
		return err
	}
	defer cl.CloseIdleConnections()
	return bootstrapAdminWith(ctx, cl, instanceAdminURL(label, r.Namespace), want, r.Log)
}

// adminPassword reads the value pinned for this instance. It is generated once by
// ensureConfig and never rotated here: the instance's own config.yml does not
// carry it, so the Secret is the only record, and regenerating it would lock the
// tenant out of an account they may already be using.
func (r *Reconciler) adminPassword(ctx context.Context, label string) (string, error) {
	name := ConfigSecretName(label)
	var sec corev1.Secret
	if err := r.Client.Get(ctx, "v1", r.Namespace, "secrets", name, &sec); err != nil {
		return "", fmt.Errorf("read %s for the administrator password: %w", name, err)
	}
	pw := string(sec.Data["admin_password"])
	if pw == "" {
		return "", fmt.Errorf("takoperator: %s has no admin_password; refusing to leave the instance on the default", name)
	}
	return pw, nil
}

// instanceAdminURL is the admin gateway's base address.
//
// Deliberately NOT ServiceHost(label): that returns a host:port for the Hub to
// dial the CoT stream on, port 8089, and pointing an HTTP client at the TLS CoT
// listener fails in a way that takes a while to understand. The host must also
// match a SAN on the certificate ensureTLS mints, which is why the namespace
// comes from the reconciler rather than the package default.
func instanceAdminURL(label, namespace string) string {
	return "https://" + instanceAdminHost(label, namespace) + ":" + strconv.Itoa(int(AdminPort))
}

func instanceAdminHost(label, namespace string) string {
	return InstanceName(label) + "." + namespace + ".svc"
}

// instanceAdminClient builds a client that trusts only the tenant's CA and
// presents a certificate that CA signed, naming the Hub identity.
//
// The certificate is minted per call and never stored. A one-day life would be
// generous; HubValidDays is used for consistency with every other Hub identity,
// and the material is thrown away when the client is.
func instanceAdminClient(ca *CA, label, namespace string) (*http.Client, error) {
	certPEM, keyPEM, err := ca.IssueClientCert(HubIdentityCN, HubValidDays)
	if err != nil {
		return nil, fmt.Errorf("mint the operator's client certificate: %w", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("takoperator: the minted client certificate does not load: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		return nil, errors.New("takoperator: the tenant CA certificate is not usable as a trust anchor")
	}
	return &http.Client{
		Timeout: adminHTTPTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:   tls.VersionTLS12,
				RootCAs:      pool,
				Certificates: []tls.Certificate{pair},
				// The instance's certificate is signed by the tenant CA for the
				// in-cluster service name, so verification is real here rather
				// than skipped: this is one of the few places where both ends
				// have the same private trust anchor.
				ServerName: instanceAdminHost(label, namespace),
			},
			DisableKeepAlives: true,
		},
	}, nil
}

// bootstrapAdminWith is the logic, separated from the k8s and TLS plumbing so it
// can be tested against an ordinary test server.
func bootstrapAdminWith(ctx context.Context, cl *http.Client, base, want string, log *slog.Logger) error {
	tok, err := otsLogin(ctx, cl, base, defaultOTSAdminPassword)
	if err != nil {
		return fmt.Errorf("probing the default administrator password: %w", err)
	}
	if tok == "" {
		// The default is already refused. Confirm the value we hold is the one
		// that works, so that a password changed outside the operator is visible
		// now rather than during an incident.
		stored, err := otsLogin(ctx, cl, base, want)
		if err != nil {
			return fmt.Errorf("checking the stored administrator password: %w", err)
		}
		if stored == "" {
			return errors.New("takoperator: administrator accepts neither the default nor the stored " +
				"password, so it was changed outside the operator; the stored value is wrong")
		}
		return nil
	}

	if err := otsResetPassword(ctx, cl, base, tok, want); err != nil {
		return err
	}

	// Verify rather than trust the 200. A reset that reports success and leaves
	// the password in place is indistinguishable from a working one in the logs,
	// and that is exactly how the inert file went unnoticed.
	again, err := otsLogin(ctx, cl, base, defaultOTSAdminPassword)
	if err != nil {
		return fmt.Errorf("verifying the default password no longer works: %w", err)
	}
	if again != "" {
		return ErrAdminPasswordStillDefault
	}
	if log != nil {
		log.Info("takoperator: administrator password changed from OpenTAKServer's default")
	}
	return nil
}

// otsLogin returns a session token, or an empty string when the credentials were
// refused. A refusal is an ANSWER here, not an error: asking "does the default
// still work" is the whole point, and folding a 400 into err would make the
// caller unable to tell "no" from "could not ask".
func otsLogin(ctx context.Context, cl *http.Client, base, password string) (string, error) {
	raw, status, err := otsPost(ctx, cl, base+"/api/login?include_auth_token", "", map[string]string{
		"username": otsAdminUsername,
		"password": password,
	})
	if err != nil {
		return "", err
	}
	switch status {
	case http.StatusOK:
		var out struct {
			Response struct {
				User struct {
					AuthenticationToken string `json:"authentication_token"`
				} `json:"user"`
			} `json:"response"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return "", fmt.Errorf("takoperator: login returned 200 with unreadable JSON: %w", err)
		}
		if out.Response.User.AuthenticationToken == "" {
			return "", errors.New("takoperator: login returned 200 with no authentication token")
		}
		return out.Response.User.AuthenticationToken, nil
	case http.StatusBadRequest, http.StatusUnauthorized:
		return "", nil
	default:
		return "", fmt.Errorf("takoperator: login: unexpected status %d", status)
	}
}

func otsResetPassword(ctx context.Context, cl *http.Client, base, token, newPassword string) error {
	raw, status, err := otsPost(ctx, cl, base+"/api/user/password/reset", token, map[string]string{
		"username":     otsAdminUsername,
		"new_password": newPassword,
	})
	if err != nil {
		return fmt.Errorf("resetting the administrator password: %w", err)
	}
	if status != http.StatusOK {
		// The body carries the reason (too short, forbidden character, no such
		// user) and is written for a person, so it is worth surfacing.
		return fmt.Errorf("takoperator: password reset returned %d: %s", status, firstLine(raw))
	}
	var out struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("takoperator: password reset returned 200 with unreadable JSON: %w", err)
	}
	if !out.Success {
		return fmt.Errorf("takoperator: password reset reported failure: %s", out.Error)
	}
	return nil
}

func otsPost(ctx context.Context, cl *http.Client, url, token string, body map[string]string) ([]byte, int, error) {
	enc, err := json.Marshal(body)
	if err != nil {
		return nil, 0, fmt.Errorf("takoperator: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(enc))
	if err != nil {
		return nil, 0, fmt.Errorf("takoperator: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		// flask-security's header, not a bearer token.
		req.Header.Set("Authentication-Token", token)
	}
	resp, err := cl.Do(req) // #nosec G704 -- in-cluster service name built from the instance label
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	// Bounded: this is third-party code answering, and an unbounded read is how a
	// reconcile loop gets to exhaust the operator's memory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("takoperator: read response: %w", err)
	}
	return raw, resp.StatusCode, nil
}

// firstLine keeps an error message to one line: OpenTAKServer answers some
// failures with an HTML page, and a log entry is not the place for it.
func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 200
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
