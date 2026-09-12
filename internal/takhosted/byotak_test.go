package takhosted

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/integrations"
)

// Bring your own TAK server (MESHSAT-1065): a tenant's stored settings turned into
// a dialable upstream, refusing what cannot work with a reason they can act on.

// selfSignedPair makes a certificate and key a tenant might paste in.
func selfSignedPair(t *testing.T, cn string) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

func goodTAKValues(t *testing.T) map[string]string {
	t.Helper()
	cert, key := selfSignedPair(t, "hub-client")
	ca, _ := selfSignedPair(t, "customer-tak-ca")
	return map[string]string{
		takFieldHost:       "tak.example.test",
		takFieldPort:       "8089",
		takFieldCA:         ca,
		takFieldClientCert: cert,
		takFieldClientKey:  key,
	}
}

// The field keys are duplicated in this package because it does not import
// internal/integrations. Pin them against the real spec, so a rename there cannot
// silently stop every tenant's own server from resolving -- the Hub would read
// absent values, find nothing configured, and forward nowhere, with no error.
func TestTheTAKFieldKeysMatchTheIntegrationsSpec(t *testing.T) {
	spec, ok := integrations.SpecFor(integrations.ProviderTAK)
	if !ok {
		t.Fatal("internal/integrations has no ProviderTAK spec; this package resolves nothing without it")
	}
	have := map[string]bool{}
	for _, f := range spec.Fields {
		have[f.Key] = true
	}
	for _, want := range []string{
		takFieldHost, takFieldPort, takFieldServerName,
		takFieldCA, takFieldClientCert, takFieldClientKey,
	} {
		if !have[want] {
			t.Errorf("the spec has no field %q; this package reads it, so the value would always "+
				"be empty and the tenant's server would never be used", want)
		}
	}
	// And the secret one really is marked secret: a private key must be
	// write-only, like every other credential on that page.
	for _, f := range spec.Fields {
		if f.Key == takFieldClientKey && !f.Secret {
			t.Error("the client key field is not marked Secret, so it would be readable back")
		}
	}
}

func TestAGoodConfigurationBecomesADialableUpstream(t *testing.T) {
	up, why := buildExternalTenant("t-acme", goodTAKValues(t))
	if why != "" {
		t.Fatalf("refused a good configuration: %s", why)
	}
	if up.Upstream != "tak.example.test:8089" {
		t.Errorf("upstream = %q, want host:port joined", up.Upstream)
	}
	if up.TenantID != "t-acme" {
		t.Errorf("tenant = %q", up.TenantID)
	}
	if up.UpstreamCAs == nil {
		t.Error("no trust pool: the Hub would have nothing to verify the server against")
	}
	if len(up.Identity.Certificate) == 0 {
		t.Error("no client identity: the server would refuse the connection")
	}
	// The issuer-index field must stay nil. Setting it would imply this upstream
	// belongs in the front's directory, which requires issuer uniqueness -- and two
	// tenants may perfectly well present the same CA for their own servers.
	if up.CA != nil {
		t.Error("CA is set; an external upstream must never enter the front's issuer index")
	}
	// Empty by default, which asks for chain verification with the name check
	// skipped.
	if up.UpstreamServerName != "" {
		t.Errorf("server name = %q, want empty unless the tenant set one", up.UpstreamServerName)
	}
}

func TestACustomCertificateNameIsHonoured(t *testing.T) {
	v := goodTAKValues(t)
	v[takFieldServerName] = "cot.acme.example"
	up, why := buildExternalTenant("t-acme", v)
	if why != "" {
		t.Fatalf("refused: %s", why)
	}
	if up.UpstreamServerName != "cot.acme.example" {
		t.Errorf("server name = %q, want the configured one so the name check runs", up.UpstreamServerName)
	}
}

// Every refusal names what is wrong, because these values came from a form a
// customer filled in and the only other place the failure surfaces is a TLS error
// in a log they cannot read.
func TestAnUnusableConfigurationIsRefusedWithAReason(t *testing.T) {
	cases := []struct {
		name string
		mut  func(map[string]string)
		want string
	}{
		{"no host", func(v map[string]string) { delete(v, takFieldHost) }, "host"},
		{"no port", func(v map[string]string) { delete(v, takFieldPort) }, "port"},
		{"port is not a number", func(v map[string]string) { v[takFieldPort] = "eight-oh-eight-nine" }, "number"},
		{"port out of range", func(v map[string]string) { v[takFieldPort] = "70000" }, "number"},
		{"no client certificate", func(v map[string]string) { delete(v, takFieldClientCert) }, "client certificate"},
		{"no client key", func(v map[string]string) { delete(v, takFieldClientKey) }, "client certificate"},
		{"no CA", func(v map[string]string) { delete(v, takFieldCA) }, "CA"},
		{"CA is not a certificate", func(v map[string]string) { v[takFieldCA] = "-----BEGIN NONSENSE-----" }, "PEM"},
		{"key does not match the certificate", func(v map[string]string) {
			_, other := selfSignedPair(t, "someone-else")
			v[takFieldClientKey] = other
		}, "pair"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := goodTAKValues(t)
			tc.mut(v)
			up, why := buildExternalTenant("t-acme", v)
			if up != nil {
				t.Fatalf("accepted an unusable configuration")
			}
			if why == "" {
				t.Fatal("refused with no reason, so nobody can fix it")
			}
			if !strings.Contains(strings.ToLower(why), strings.ToLower(tc.want)) {
				t.Errorf("reason = %q, want it to mention %q", why, tc.want)
			}
		})
	}
}

// --- the resolver over a source -------------------------------------------

type fakeTAKSource struct {
	values map[string]map[string]string
	err    error
	calls  int
}

func (f *fakeTAKSource) TAKUpstream(_ context.Context, tenantID string) (map[string]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.values[tenantID], nil
}

// Most tenants will never configure this. That is not an error and must not be
// logged or retried per position.
func TestATenantWithNoOwnServerResolvesNothingQuietly(t *testing.T) {
	src := &fakeTAKSource{values: map[string]map[string]string{}}
	e := NewExternalUpstreams(src, nil)

	up, why := e.For(context.Background(), "t-none")
	if up != nil || why != "" {
		t.Errorf("got (%v, %q), want nothing and no reason", up, why)
	}
}

func TestAConfiguredTenantResolvesAndIsCached(t *testing.T) {
	src := &fakeTAKSource{values: map[string]map[string]string{"t-acme": goodTAKValues(t)}}
	e := NewExternalUpstreams(src, nil)
	ctx := context.Background()

	up, why := e.For(ctx, "t-acme")
	if up == nil {
		t.Fatalf("did not resolve: %s", why)
	}
	if src.calls != 1 {
		t.Fatalf("source calls = %d, want 1", src.calls)
	}
	// Again: served from cache, because this is asked per position and each miss
	// decrypts a row and parses a chain.
	if up2, _ := e.For(ctx, "t-acme"); up2 == nil {
		t.Fatal("second call resolved nothing")
	}
	if src.calls != 1 {
		t.Errorf("source calls = %d after two resolves, want 1: the cache is not working", src.calls)
	}

	// And Forget makes it ask again, for when the customer changes their settings.
	e.Forget("t-acme")
	if _, _ = e.For(ctx, "t-acme"); src.calls != 2 {
		t.Errorf("source calls = %d after Forget, want 2", src.calls)
	}
}

// A read failure is transient and must NOT be cached: caching it would keep a
// tenant's traffic off their own server for the whole interval after one blip.
func TestAReadFailureIsNotCached(t *testing.T) {
	src := &fakeTAKSource{err: errors.New("the database is having a moment")}
	e := NewExternalUpstreams(src, nil)
	ctx := context.Background()

	if _, why := e.For(ctx, "t-acme"); why == "" {
		t.Error("a read failure produced no reason")
	}
	if _, _ = e.For(ctx, "t-acme"); src.calls != 2 {
		t.Errorf("source calls = %d, want 2: a transient failure was cached", src.calls)
	}
}

func TestANilSourceResolvesNothing(t *testing.T) {
	e := NewExternalUpstreams(nil, nil)
	if up, why := e.For(context.Background(), "t-acme"); up != nil || why != "" {
		t.Errorf("got (%v, %q), want nothing", up, why)
	}
}
