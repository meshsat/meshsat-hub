package integrations

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

// selfSigned returns a certificate and its key as PEM text.
func selfSigned(t *testing.T, cn string) (certPEM, keyPEM string) {
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

// A PEM block pasted wrong is refused at save time with a sentence, not at
// connect time inside a retry loop (MESHSAT-1151).
func TestPEMFieldsAreValidatedOnSave(t *testing.T) {
	svc, ctx := newSvc(t)
	svc.DisableURLCheckForTest()
	ca, _ := selfSigned(t, "broker-ca")
	cert, key := selfSigned(t, "client")
	_, otherKey := selfSigned(t, "other")

	base := map[string]string{"api_key": "k", "account_id": "acct-1", "mqtt_broker_url": "ssl://mqtt.example.net:8883"}
	with := func(kv map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range kv {
			m[k] = v
		}
		return m
	}

	// The API key alone is a complete account: the feed fields are optional.
	if _, err := svc.Set(ctx, "t-1", ProviderCloudloop, map[string]string{"api_key": "k"}); err != nil {
		t.Fatalf("an account without the MQTT feed must save: %v", err)
	}
	// A whole valid set saves.
	if _, err := svc.Set(ctx, "t-1", ProviderCloudloop, with(map[string]string{"mqtt_ca_pem": ca, "mqtt_client_cert_pem": cert, "mqtt_client_key_pem": key})); err != nil {
		t.Fatalf("a valid CA, certificate and key were refused: %v", err)
	}
	// Newlines stripped (what a single-line input does to a paste) is refused.
	flat := strings.ReplaceAll(ca, "\n", " ")
	if _, err := svc.Set(ctx, "t-2", ProviderCloudloop, with(map[string]string{"mqtt_ca_pem": flat})); err == nil || !strings.Contains(err.Error(), "PEM") {
		t.Fatalf("a flattened PEM block was accepted: err=%v", err)
	}
	// A certificate with somebody else's key is refused.
	if _, err := svc.Set(ctx, "t-3", ProviderCloudloop, with(map[string]string{"mqtt_ca_pem": ca, "mqtt_client_cert_pem": cert, "mqtt_client_key_pem": otherKey})); err == nil || !strings.Contains(err.Error(), "do not go together") {
		t.Fatalf("a certificate and a foreign key were accepted: err=%v", err)
	}
	// The TAK provider's PEM fields get the same check.
	if _, err := svc.Set(ctx, "t-4", ProviderTAK, map[string]string{"host": "tak.example.net", "ca_pem": "not pem", "client_cert_pem": cert, "client_key_pem": key}); err == nil {
		t.Fatal("a TAK server CA that is not PEM was accepted")
	}
	// A multiline field may exceed the single-line cap (a chain is > 4 KiB).
	long := ca + strings.Repeat("\n", 0)
	for len(long) < 5000 {
		long += ca
	}
	if _, err := svc.Set(ctx, "t-5", ProviderCloudloop, with(map[string]string{"mqtt_ca_pem": long})); err != nil {
		t.Fatalf("a 5 KiB CA chain was refused: %v", err)
	}
	if _, err := svc.Set(ctx, "t-6", ProviderCloudloop, with(map[string]string{"api_url": "https://" + strings.Repeat("a", 4100) + ".example.net"})); err == nil {
		t.Fatal("a 4 KiB single-line field was accepted")
	}
}

func TestPEMFieldsRenderMultiline(t *testing.T) {
	for _, provider := range []string{ProviderCloudloop, ProviderTAK} {
		spec, _ := SpecFor(provider)
		for _, f := range spec.Fields {
			if f.PEM && !f.Multiline {
				t.Errorf("%s.%s is a PEM field rendered as a single-line input; the paste loses its newlines", provider, f.Key)
			}
		}
	}
}

// The broker address is dialled by the Hub from inside the cluster, so it
// gets the same public-address guard as a webhook URL, with MQTT schemes.
func TestBrokerFieldRefusesInternalAddresses(t *testing.T) {
	svc, ctx := newSvc(t) // URL check ON
	for _, bad := range []string{"ssl://nats:1883", "ssl://10.0.0.5:8883", "http://mqtt.example.net:8883", "ssl://localhost:8883", "mqtt.example.net:8883"} {
		if _, err := svc.Set(ctx, "t-b", ProviderCloudloop, map[string]string{"api_key": "k", "mqtt_broker_url": bad}); err == nil {
			t.Errorf("broker %q was accepted", bad)
		}
	}
}
