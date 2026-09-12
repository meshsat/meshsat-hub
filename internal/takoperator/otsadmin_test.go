package takoperator

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeOTS is enough of OpenTAKServer's auth surface to drive the bootstrap: an
// account with a password, flask-security's login shape, and the admin reset
// endpoint behind a token.
type fakeOTS struct {
	password string
	// refuseReset makes the reset endpoint answer 200 {"success": true} while
	// changing nothing, which is the failure mode that looks like success.
	refuseReset bool
	resets      int
	logins      []string
}

func (f *fakeOTS) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Username, Password string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.logins = append(f.logins, in.Password)
		w.Header().Set("Content-Type", "application/json")
		if in.Username != otsAdminUsername || in.Password != f.password {
			// What flask-security actually answers for a bad credential.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"response":{"errors":{"password":["Invalid password"]}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"response":{"user":{"authentication_token":"tok-123"}}}`))
	})
	mux.HandleFunc("/api/user/password/reset", func(w http.ResponseWriter, r *http.Request) {
		f.resets++
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authentication-Token") != "tok-123" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"error":"no token"}`))
			return
		}
		var in struct {
			Username    string `json:"username"`
			NewPassword string `json:"new_password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if !f.refuseReset {
			f.password = in.NewPassword
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	return mux
}

func TestTheAdminBootstrapReplacesTheDefaultPasswordAndProvesIt(t *testing.T) {
	ots := &fakeOTS{password: defaultOTSAdminPassword}
	srv := httptest.NewServer(ots.handler())
	defer srv.Close()

	if err := bootstrapAdminWith(context.Background(), srv.Client(), srv.URL, "a-generated-value", nil); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}
	if ots.password != "a-generated-value" {
		t.Errorf("the instance's password is %q, want the generated value", ots.password)
	}
	if ots.resets != 1 {
		t.Errorf("reset called %d times, want exactly 1", ots.resets)
	}
	// It must END by asking whether the default still works. Without that check a
	// reset that silently does nothing reads as success — which is precisely how
	// the inert .admin_password file survived review.
	if last := ots.logins[len(ots.logins)-1]; last != defaultOTSAdminPassword {
		t.Errorf("the last login attempt used %q; the bootstrap must finish by re-testing the DEFAULT", last)
	}
}

// Mutation-checked: deleting the verification step in bootstrapAdminWith makes
// this test pass silently, which is why it asserts the error identity.
func TestTheAdminBootstrapFailsWhenTheResetDidNothing(t *testing.T) {
	ots := &fakeOTS{password: defaultOTSAdminPassword, refuseReset: true}
	srv := httptest.NewServer(ots.handler())
	defer srv.Close()

	err := bootstrapAdminWith(context.Background(), srv.Client(), srv.URL, "a-generated-value", nil)
	if !errors.Is(err, ErrAdminPasswordStillDefault) {
		t.Fatalf("error = %v, want ErrAdminPasswordStillDefault: a reset that reports success and "+
			"changes nothing must not be mistaken for a finished job", err)
	}
}

// A second pass over an instance that is already done must be cheap and silent.
func TestTheAdminBootstrapIsIdempotent(t *testing.T) {
	ots := &fakeOTS{password: "a-generated-value"}
	srv := httptest.NewServer(ots.handler())
	defer srv.Close()

	if err := bootstrapAdminWith(context.Background(), srv.Client(), srv.URL, "a-generated-value", nil); err != nil {
		t.Fatalf("a second pass should succeed quietly, got: %v", err)
	}
	if ots.resets != 0 {
		t.Errorf("reset was called %d times on an already-bootstrapped instance, want 0", ots.resets)
	}
}

// If neither password works, somebody changed it by hand. Saying so beats
// reporting success, and beats resetting it back and locking that person out.
func TestTheAdminBootstrapSaysSoWhenThePasswordWasChangedElsewhere(t *testing.T) {
	ots := &fakeOTS{password: "changed-by-a-person"}
	srv := httptest.NewServer(ots.handler())
	defer srv.Close()

	err := bootstrapAdminWith(context.Background(), srv.Client(), srv.URL, "a-generated-value", nil)
	if err == nil {
		t.Fatal("want an error when neither the default nor the stored password works")
	}
	if !strings.Contains(err.Error(), "outside the operator") {
		t.Errorf("error %q does not say the password was changed outside the operator", err)
	}
	if ots.resets != 0 {
		t.Errorf("reset was called %d times; the operator must not overwrite a password it did not set", ots.resets)
	}
}

// The admin gateway is on 8444. ServiceHost returns ...:8089 for the CoT stream,
// and using it here would point an HTTP client at the TLS CoT listener — a
// failure that costs an hour to read. This asserts the two never converge.
func TestTheAdminURLUsesTheAdminPortAndNotTheCoTPort(t *testing.T) {
	const label = "abcdefghij"
	url := instanceAdminURL(label, DefaultNamespace)

	if !strings.HasPrefix(url, "https://") {
		t.Errorf("admin URL %q is not https; the gateway offers no plaintext listener at all", url)
	}
	if !strings.HasSuffix(url, ":8444") {
		t.Errorf("admin URL %q does not end in the admin port 8444", url)
	}
	if strings.Contains(url, "8089") {
		t.Errorf("admin URL %q carries the CoT port", url)
	}
	if strings.Contains(url, ServiceHost(label)) {
		t.Errorf("admin URL %q was built from ServiceHost, which carries :8089", url)
	}
	// The host must match a SAN on the certificate ensureTLS mints, which are
	// tak-<label>, tak-<label>.<ns> and tak-<label>.<ns>.svc.
	if want := InstanceName(label) + "." + DefaultNamespace + ".svc"; instanceAdminHost(label, DefaultNamespace) != want {
		t.Errorf("admin host = %q, want %q so TLS verification matches the issued SAN",
			instanceAdminHost(label, DefaultNamespace), want)
	}
}

// The operator's own certificate is an admin credential. It must name the Hub
// identity the nginx matches, and it must be client-auth only: the same CA signs
// every phone in the tenant, so anything sloppier here turns a phone into an
// administrator.
func TestTheOperatorsClientCertificateIsTheHubIdentityAndClientAuthOnly(t *testing.T) {
	certPEM, keyPEM, err := NewTenantCA("abcdefghij")
	if err != nil {
		t.Fatalf("new CA: %v", err)
	}
	ca, err := LoadCA(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("load CA: %v", err)
	}
	cl, err := instanceAdminClient(ca, "abcdefghij", DefaultNamespace)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	defer cl.CloseIdleConnections()

	tr, ok := cl.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", cl.Transport)
	}
	cfg := tr.TLSClientConfig
	if cfg == nil {
		t.Fatal("the client has no TLS configuration")
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify is set; both ends share the tenant CA here, so verification must be real")
	}
	if cfg.RootCAs == nil {
		t.Error("no root pool: the client would trust the public CA set instead of the tenant's")
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("client presents %d certificates, want 1", len(cfg.Certificates))
	}
	block, _ := pem.Decode(mustIssuedClientPEM(t, ca))
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse issued certificate: %v", err)
	}
	if leaf.Subject.CommonName != HubIdentityCN {
		t.Errorf("common name = %q, want %q — the gateway matches that exact component",
			leaf.Subject.CommonName, HubIdentityCN)
	}
	for _, u := range leaf.ExtKeyUsage {
		if u == x509.ExtKeyUsageServerAuth {
			t.Error("the certificate carries serverAuth; the operator is a client here and nothing else")
		}
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("extended key usage = %v, want clientAuth only", leaf.ExtKeyUsage)
	}
	if leaf.DNSNames != nil {
		t.Errorf("the client certificate carries DNS names %v; it identifies a caller, not a host", leaf.DNSNames)
	}
}

func mustIssuedClientPEM(t *testing.T, ca *CA) []byte {
	t.Helper()
	certPEM, _, err := ca.IssueClientCert(HubIdentityCN, HubValidDays)
	if err != nil {
		t.Fatalf("issue client certificate: %v", err)
	}
	return certPEM
}

// The nginx gateway must expose the login endpoint, or the admin slice behind it
// is reachable and unusable: every /api/user/ route is gated on
// @roles_accepted("administrator"), so a caller with no way to log in can do
// nothing at all. That is how it first shipped.
func TestTheAdminGatewayExposesLoginAndStillRefusesEverythingElse(t *testing.T) {
	cfg := nginxConfig()

	if !strings.Contains(cfg, "location = /api/login") {
		t.Error("the gateway does not proxy /api/login, so no caller can obtain the token " +
			"that /api/user/ requires")
	}
	// Exact match only: /api/login-something must not ride along.
	if strings.Contains(cfg, "location /api/login ") || strings.Contains(cfg, "location ^~ /api/login") {
		t.Error("/api/login is matched as a prefix; it must be an exact location")
	}
	// Every proxied location stays behind the client-certificate check.
	for _, path := range []string{"/api/login", "/api/user/", "/api/groups"} {
		idx := strings.Index(cfg, path+" {")
		if idx < 0 {
			idx = strings.Index(cfg, path+" ")
		}
		if idx < 0 {
			t.Errorf("location for %q is missing entirely", path)
			continue
		}
		rest := cfg[idx:]
		if end := strings.Index(rest, "}"); end > 0 {
			if !strings.Contains(rest[:end], "$is_hub = 0") {
				t.Errorf("location %q is not gated on the Hub's common name", path)
			}
		}
	}
	// And the things that must never be reachable.
	for _, forbidden := range []string{"/api/config", "/api/truststore", "/Marti", "/oauth", "/socket.io"} {
		if strings.Contains(cfg, "location "+forbidden) {
			t.Errorf("the gateway names %q as a location; it must fall to the default 404", forbidden)
		}
	}
}
