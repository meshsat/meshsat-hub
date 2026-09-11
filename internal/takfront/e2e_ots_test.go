package takfront

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"
)

// TestEndToEndAgainstARealOpenTAKServer proves the design against a real
// OpenTAKServer rather than the fake in server_test.go: two phones, each with
// its own certificate from the tenant's CA, reach the tenant's server through
// one front port and see each other's CoT, while the server sees a single
// identity it issued.
//
// It is skipped unless an OpenTAKServer is pointed at, because CI has none:
//
//	kubectl -n meshsat-tak-spike port-forward deploy/ots-spike 18089:8089 &
//	TAKFRONT_E2E_UPSTREAM=127.0.0.1:18089 \
//	TAKFRONT_E2E_UPSTREAM_CA=ots-ca.pem \
//	TAKFRONT_E2E_UPSTREAM_CERT=proxy-cert.pem \
//	TAKFRONT_E2E_UPSTREAM_KEY=proxy-key.pem \
//	go test ./internal/takfront/ -run EndToEnd -v
//
// The certificate and key are the ONE OpenTAKServer user the whole tenant is
// proxied as; see scratchpad setup_proxy.py in MESHSAT-1046 for how they are
// minted. The phones' CA is generated here and is deliberately NOT the
// OpenTAKServer CA: the point is that the tenant is identified by an issuer the
// Hub controls, with the tenant's server knowing nothing about it.
func TestEndToEndAgainstARealOpenTAKServer(t *testing.T) {
	addr := os.Getenv("TAKFRONT_E2E_UPSTREAM")
	if addr == "" {
		t.Skip("set TAKFRONT_E2E_UPSTREAM to run against a real OpenTAKServer")
	}
	identity, err := tls.LoadX509KeyPair(
		envFile(t, "TAKFRONT_E2E_UPSTREAM_CERT"), envFile(t, "TAKFRONT_E2E_UPSTREAM_KEY"))
	if err != nil {
		t.Fatalf("load upstream identity: %v", err)
	}
	caPEM, err := os.ReadFile(envFile(t, "TAKFRONT_E2E_UPSTREAM_CA"))
	if err != nil {
		t.Fatalf("read upstream CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("upstream CA file held no certificate")
	}

	// The tenant CA the Hub would hold and issue phone certificates from.
	tenantCA := newTestCA(t, "meshsat tenant e2e CA")
	f := startFront(t, Config{
		IdleTimeout: 2 * time.Minute,
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}, allowAll{}, []Tenant{{
		TenantID:    "e2e",
		Label:       "e2espike",
		CA:          tenantCA.cert,
		Upstream:    addr,
		Identity:    identity,
		UpstreamCAs: pool,
	}})

	one, err := f.dial(t, tenantCA.issue(t, "phoneone"))
	if err != nil {
		t.Fatalf("first phone: %v", err)
	}
	two, err := f.dial(t, tenantCA.issue(t, "phonetwo"))
	if err != nil {
		t.Fatalf("second phone: %v", err)
	}

	// Each phone announces itself first: OpenTAKServer declares a queue per
	// callsign from the traffic, which is why two phones sharing one upstream
	// identity still get their own relay.
	if _, err := one.Write(cotXML("e2e-one", "E2E-ONE", 52.1, 4.5, "")); err != nil {
		t.Fatalf("first phone write: %v", err)
	}
	if _, err := two.Write(cotXML("e2e-two", "E2E-TWO", 52.2, 4.6, "")); err != nil {
		t.Fatalf("second phone write: %v", err)
	}

	// Resend the marked event while draining, rather than sleeping a guessed
	// interval: the server needs a moment to declare both queues.
	marker := "e2e-" + randomHex(t)
	deadline := time.Now().Add(30 * time.Second)
	buf := make([]byte, 32*1024)
	var got []byte
	for time.Now().Before(deadline) {
		if _, err := one.Write(cotXML("e2e-one", "E2E-ONE", 52.3, 4.7,
			"<remarks>"+marker+"</remarks>")); err != nil {
			t.Fatalf("marked write: %v", err)
		}
		if err := two.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("set deadline: %v", err)
		}
		n, err := two.Read(buf)
		got = append(got, buf[:n]...)
		if bytes.Contains(got, []byte(marker)) {
			t.Logf("the second phone received the first phone's CoT through the real server (%d bytes)", len(got))
			break
		}
		if err != nil && !isClosed(err) {
			t.Fatalf("second phone read: %v", err)
		}
	}
	if !bytes.Contains(got, []byte(marker)) {
		t.Fatalf("the second phone never received %q; it read %d bytes: %.400q", marker, len(got), got)
	}

	// The tenant's own CA is irrelevant to the front: a certificate it signed
	// is not a certificate this tenant's phones are issued from.
	stranger := newTestCA(t, "another deployment CA")
	if conn, err := f.dial(t, stranger.issue(t, "mallory")); err == nil {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Read(make([]byte, 1)); err == nil {
			t.Error("a certificate from an unknown CA reached the real server")
		}
	}

	// The refusal is recorded after the connection is closed, so the stranger
	// having seen the close does not mean it has been counted yet.
	waitFor(t, "the stranger's refusal to be recorded", func() bool {
		refused, _, _ := f.rec.state()
		return len(refused) == 1
	})
	refused, opened, _ := f.rec.state()
	if len(opened) != 2 {
		t.Errorf("opened = %v, want one per phone", opened)
	}
	if refused[0] != "/"+reasonHandshake {
		t.Errorf("refused = %v, want only the stranger's handshake", refused)
	}
}

func envFile(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set", name)
	}
	return v
}

func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b)
}

// cotXML is a situational-awareness event in the shape ATAK sends, including the
// contact and group detail OpenTAKServer relays on.
func cotXML(uid, callsign string, lat, lon float64, extra string) []byte {
	now := time.Now().UTC()
	stamp := now.Format("2006-01-02T15:04:05.000Z")
	stale := now.Add(5 * time.Minute).Format("2006-01-02T15:04:05.000Z")
	return []byte(fmt.Sprintf(
		`<event version="2.0" uid="%s" type="a-f-G-U-C" how="m-g" time="%s" start="%s" stale="%s">`+
			`<point lat="%f" lon="%f" hae="0" ce="10" le="10"/>`+
			`<detail><contact callsign="%s"/>`+
			`<takv device="meshsat-e2e" platform="ATAK-CIV" os="e2e" version="5.0"/>`+
			`<__group name="Cyan" role="Team Member"/>%s</detail></event>`+"\n",
		uid, stamp, stamp, stale, lat, lon, callsign, extra))
}
