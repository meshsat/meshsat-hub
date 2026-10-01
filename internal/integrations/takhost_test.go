package integrations

import (
	"strings"
	"testing"
)

// A tenant's own TAK server is dialled by the Hub from inside the cluster, as raw
// TLS. Its address is not a URL, so it never went through the URL check, and a
// tenant could point the Hub at the database or the broker by name (MESHSAT-1460).
func TestATenantCannotPointTheHubAtAnInClusterTAKHost(t *testing.T) {
	svc, ctx := newSvc(t) // the save-time check is ON
	cert, key := selfSigned(t, "hub-client")
	ca, _ := selfSigned(t, "customer-tak-ca")
	with := func(host string) map[string]string {
		return map[string]string{
			"host": host, "port": "8089",
			"ca_pem": ca, "client_cert_pem": cert, "client_key_pem": key,
		}
	}

	for _, bad := range []string{
		"tak-becf7d519c.meshsat-tak.svc",
		"meshsat-hub-main-rw.meshsat-hub-db.svc.cluster.local",
		"nats",
		"localhost",
		"127.0.0.1",
		"10.2.5.119",
		"169.254.169.254",
		"[::1]",
		"tak.example.net:8089",
		"https://tak.example.net",
	} {
		_, err := svc.Set(ctx, "t-bad", ProviderTAK, with(bad))
		if err == nil {
			t.Errorf("a TAK server at %q was accepted", bad)
			continue
		}
		// The customer is told which field, in the form's own word for it.
		if !strings.Contains(err.Error(), "Host") {
			t.Errorf("the refusal of %q does not name the field: %v", bad, err)
		}
	}

	// The control, so the loop above is not passing on a form that saves nothing:
	// the same values with a public address are accepted.
	if _, err := svc.Set(ctx, "t-ok", ProviderTAK, with("93.184.216.34")); err != nil {
		t.Fatalf("a TAK server at a public address was refused: %v", err)
	}
}

// The check is driven by the field's own flag, so a provider added later inherits
// it. This pins the flag on the one field that needs it today.
func TestTheTAKHostFieldIsMarkedForTheAddressCheck(t *testing.T) {
	spec, ok := SpecFor(ProviderTAK)
	if !ok {
		t.Fatal("no TAK provider spec")
	}
	for _, f := range spec.Fields {
		if f.Key == "host" {
			if !f.Host {
				t.Error("the TAK host field is not marked Host: its address would be stored unchecked")
			}
			return
		}
	}
	t.Fatal("the TAK provider has no host field")
}
