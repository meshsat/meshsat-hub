package cloudloop

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/metrics"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// counterValue reads a counter without prometheus/testutil, which is not a
// dependency of this module.
func counterValue(c prometheus.Counter) float64 {
	var m dto.Metric
	_ = c.Write(&m)
	return m.GetCounter().GetValue()
}

func testPEM(t *testing.T, cn string) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
}

// fakeAccounts is the integrations service reduced to what the pool reads.
type fakeAccounts struct {
	rows map[string]*integrations.Account
}

func (f *fakeAccounts) TenantsWith(_ context.Context, _ string) ([]string, error) {
	var out []string
	for tid := range f.rows {
		out = append(out, tid)
	}
	return out, nil
}

func (f *fakeAccounts) ForTenant(_ context.Context, tid, _ string) (*integrations.Account, error) {
	return f.rows[tid], nil
}

func feedAccount(t *testing.T, tid, broker string) *integrations.Account {
	ca, _ := testPEM(t, "broker-ca-"+tid)
	cert, key := testPEM(t, "client-"+tid)
	return &integrations.Account{Provider: integrations.ProviderCloudloop, TenantID: tid, Fields: map[string]string{
		"api_key": "k", "account_id": "acct-" + tid, "mqtt_broker_url": broker,
		"mqtt_ca_pem": ca, "mqtt_client_cert_pem": cert, "mqtt_client_key_pem": key,
	}}
}

// Nothing listens on this broker address; the pool must still bring up one
// feed per tenant, keep their identities apart, count the failure, and keep
// one tenant's broken certificate from touching another's feed.
func TestMQTTPoolKeepsTenantsApart(t *testing.T) {
	const deadBroker = "ssl://127.0.0.1:1"
	accts := &fakeAccounts{rows: map[string]*integrations.Account{
		"alpha": feedAccount(t, "alpha", deadBroker),
		"beta":  feedAccount(t, "beta", deadBroker),
		// A platform account is the environment's feed, never this pool's.
		store.DefaultTenantID: {Provider: integrations.ProviderCloudloop, TenantID: store.DefaultTenantID, Platform: true,
			Fields: feedAccount(t, "platform", deadBroker).Fields},
		// An account with an API key and no MQTT fields is complete and gets no feed.
		"webhook-only": {Provider: integrations.ProviderCloudloop, TenantID: "webhook-only", Fields: map[string]string{"api_key": "k"}},
	}}
	// Somebody pasted a key that is not the certificate's.
	broken := feedAccount(t, "broken", deadBroker)
	_, otherKey := testPEM(t, "someone-else")
	broken.Fields["mqtt_client_key_pem"] = otherKey
	accts.rows["broken"] = broken

	failuresBefore := counterValue(metrics.CloudloopMQTTConnectFailures.WithLabelValues("broken"))

	p := NewMQTTPool(accts, func(context.Context, *LingoMO) string { return "ok" })
	p.dialWait = 200 * time.Millisecond
	t.Cleanup(p.Close)
	p.Reconcile(context.Background())

	feeds := p.Feeds()
	for _, tid := range []string{"alpha", "beta"} {
		if _, ok := feeds[tid]; !ok {
			t.Fatalf("no feed for %s: %+v", tid, feeds)
		}
	}
	for _, tid := range []string{store.DefaultTenantID, "webhook-only", "broken"} {
		if _, ok := feeds[tid]; ok {
			t.Fatalf("%s got a feed and must not have: %+v", tid, feeds[tid])
		}
	}
	if feeds["alpha"].ClientID == feeds["beta"].ClientID {
		t.Fatalf("two tenants share client id %q; the broker would evict one with the other", feeds["alpha"].ClientID)
	}
	if feeds["alpha"].Account != "acct-alpha" || feeds["beta"].Account != "acct-beta" {
		t.Fatalf("feeds subscribed under the wrong account: %+v", feeds)
	}
	if got := counterValue(metrics.CloudloopMQTTConnectFailures.WithLabelValues("broken")); got != failuresBefore+1 {
		t.Fatalf("the broken certificate was not counted as a failure (%v -> %v)", failuresBefore, got)
	}

	// The account is removed: the feed goes with it, the other stays.
	delete(accts.rows, "beta")
	p.Reconcile(context.Background())
	feeds = p.Feeds()
	if _, ok := feeds["beta"]; ok {
		t.Fatal("beta's feed survived the removal of its account")
	}
	if _, ok := feeds["alpha"]; !ok {
		t.Fatal("alpha's feed was dropped by beta's removal")
	}

	// A rotated certificate is a new fingerprint and a new client id.
	old := feeds["alpha"].ClientID
	accts.rows["alpha"] = feedAccount(t, "alpha", deadBroker)
	p.Reconcile(context.Background())
	if p.Feeds()["alpha"].ClientID == old {
		t.Fatal("a rotated certificate kept the old client id; the stale session would fight the new one at the broker")
	}
}

func TestTLSFromPEMRefusesTheWrongKey(t *testing.T) {
	ca, _ := testPEM(t, "ca")
	cert, key := testPEM(t, "client")
	_, other := testPEM(t, "other")
	if _, err := tlsFromPEM(ca, cert, key); err != nil {
		t.Fatalf("a matching pair was refused: %v", err)
	}
	if _, err := tlsFromPEM(ca, cert, other); err == nil {
		t.Fatal("a certificate with a foreign key was accepted")
	}
	if _, err := tlsFromPEM("not pem", cert, key); err == nil {
		t.Fatal("a CA that is not PEM was accepted")
	}
}
