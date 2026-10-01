package netguard

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The dial-time guard is what survives DNS rebinding: a name that resolved
// publicly when the URL was saved can resolve to 127.0.0.1 an hour later, and a
// registration-time check does not see that. This dials a loopback server
// DIRECTLY, which is the state rebinding produces.
func TestTheClientRefusesToConnectToLoopbackEvenWhenAskedDirectly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := SafeHTTPClient(5 * time.Second)
	resp, err := c.Get(srv.URL) //nolint:bodyclose // the request must not succeed
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("the guarded client connected to a loopback address: DNS rebinding " +
			"would walk straight past the save-time check and reach the cluster")
	}
	if !strings.Contains(err.Error(), "refusing to connect") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

// It must still reach a legitimate external address, or the guard makes the
// feature unusable and somebody removes it.
func TestTheClientStillReachesAPublicAddress(t *testing.T) {
	c := SafeHTTPClient(5 * time.Second)
	// No network call: dialling a public literal that is not listening should
	// fail with a CONNECTION error, not with the guard's refusal.
	_, err := c.Get("http://198.51.100.7:9") // TEST-NET-2, reserved for docs
	if err == nil {
		t.Skip("unexpectedly reachable")
	}
	if strings.Contains(err.Error(), "refusing to connect") {
		t.Errorf("the guard refused a public address: %v", err)
	}
}

func TestIsPublicIP(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"198.51.100.7", true},
		{"127.0.0.1", false},
		{"169.254.169.254", false}, // cloud metadata
		{"10.1.2.3", false},
		{"192.168.1.1", false},
		{"172.16.0.1", false},
		{"100.64.0.1", false}, // the cluster's own mesh
		{"::1", false},
	} {
		if got := IsPublicIP(net.ParseIP(tc.ip)); got != tc.want {
			t.Errorf("IsPublicIP(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

func TestValidatePublicURLRefusesInternalNames(t *testing.T) {
	for _, raw := range []string{
		"http://localhost:8080",
		"http://meshsat-hub-main-rw.meshsat-hub-db.svc:5432",
		"http://foo.internal",
		"file:///etc/passwd",
		"http://169.254.169.254/",
	} {
		if err := ValidatePublicURL(raw); err == nil {
			t.Errorf("ValidatePublicURL(%q) allowed it", raw)
		}
	}
	_ = context.Background()
}

// A setting that is a bare host rather than a URL gets the same answer a URL
// naming that host would (MESHSAT-1460): the address of a tenant's own TAK server
// is dialled as raw TLS, so it never had a scheme and never reached the URL check.
func TestValidatePublicHostRefusesInternalAddresses(t *testing.T) {
	for _, bad := range []string{
		"",
		"localhost",
		"tak-becf7d519c.meshsat-tak.svc",
		"meshsat-hub-main-rw.meshsat-hub-db.svc.cluster.local",
		"nats.internal",
		"127.0.0.1",
		"10.2.5.119",
		"192.168.1.10",
		"169.254.169.254",
		"100.64.0.9",
		"::1",
		"[::1]",
		"fd00::1",
		// More than a host: refused rather than interpreted.
		"tak.example.net:8089",
		"https://tak.example.net",
		"tak.example.net/path",
		"user@tak.example.net",
		"tak.example.net evil",
	} {
		if err := ValidatePublicHost(bad); err == nil {
			t.Errorf("%q was accepted as a public host", bad)
		}
	}
}

// A public address literal passes with no lookup, so the test needs no DNS.
func TestValidatePublicHostAcceptsAPublicAddress(t *testing.T) {
	for _, good := range []string{"93.184.216.34", " 93.184.216.34 ", "2606:2800:220:1:248:1893:25c8:1946"} {
		if err := ValidatePublicHost(good); err != nil {
			t.Errorf("%q was refused: %v", good, err)
		}
	}
}

// DialControl is the hook itself, for a dialer that is not HTTP.
func TestDialControlRefusesWhatTheClientRefuses(t *testing.T) {
	for addr, wantErr := range map[string]bool{
		"127.0.0.1:8089":       true,
		"10.2.5.119:8089":      true,
		"169.254.169.254:80":   true,
		"[::1]:8089":           true,
		"not-an-address:8089":  true, // Control is handed a resolved literal
		"no-port":              true,
		"93.184.216.34:8089":   false,
		"[2606:2800::1]:8089":  false,
		"100.64.0.1:8089":      true,
		"198.18.0.1:8089":      true,
		"192.0.2.1:8089":       false, // documentation range: not ours, so not refused
		"[2002:c000:201::]:80": true,
	} {
		err := DialControl("tcp", addr, nil)
		if (err != nil) != wantErr {
			t.Errorf("DialControl(%q) = %v, want refused=%v", addr, err, wantErr)
		}
	}
}
