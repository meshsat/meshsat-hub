package api

import (
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/bridge"
)

// The Bridge's relay client fetches the bridge CA from here rather than
// carrying it in its Hub connection (that field is the MQTT broker's root
// store). It must be the CA that issues bridge certificates, served as PEM,
// with no credentials, and 404 when the Hub has none.
func TestRelayCAIsServedAsPEMWithoutCredentials(t *testing.T) {
	h := NewRelayHandler(nil, nil, nil)

	rec := httptest.NewRecorder()
	h.CA(rec, httptest.NewRequest(http.MethodGet, "/api/relay/ca", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no CA: %d, want 404", rec.Code)
	}

	ca, caPEM, _, err := bridge.NewSelfSignedCA("test-hub")
	if err != nil {
		t.Fatal(err)
	}
	h.SetCA(ca.CACertPEM())
	rec = httptest.NewRecorder()
	h.CA(rec, httptest.NewRequest(http.MethodGet, "/api/relay/ca", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-pem-file" {
		t.Fatalf("content type %q", ct)
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != string(caPEM) {
		t.Fatal("served PEM is not the CA certificate")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(body) {
		t.Fatal("served body is not a parseable certificate")
	}
	// And a certificate the CA issues verifies against what was served.
	certPEM, _, err := ca.IssueBridgeCert("kit-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.VerifyBridgeCert(certPEM); err != nil {
		t.Fatal(err)
	}
}
