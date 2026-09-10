package stripe

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestStripeIsNotOnAnyIngestPath extends the SOS invariant to the payment
// provider (MESHSAT-1023).
//
// The promise is unconditional and it outranks every commercial feature: an SOS
// is never affected by billing. Not by a lapsed plan, not by being over the
// device ceiling, not by a refund, and now not by a cancelled subscription
// either. This package can end a plan on a webhook — which is more power than
// anything billing had before, because Ko-fi could never say "cancelled" — so it
// must sit further from field traffic, not closer.
//
// Like its two siblings, this reads direct imports and source text rather than
// the transitive dependency graph: some of these packages reach internal/api for
// its JSON helpers and would drag the whole handler set in. A permanently red
// test is a deleted test.
func TestStripeIsNotOnAnyIngestPath(t *testing.T) {
	const self = "github.com/meshsat/meshsat-hub/internal/stripe"
	ingest := []string{
		"rockblock",    // Iridium MO webhook
		"cloudloop",    // Cloudloop/LingoMO webhook + MT sender
		"globalstar",   // Globalstar webhook
		"sms",          // inbound SMS
		"email",        // inbound PGP email gateway
		"sos",          // SOS detection and escalation
		"deadman",      // dead man's switch
		"escalation",   // escalation chains
		"routing",      // the dispatcher
		"message",      // MO/MT subscriber
		"ratelimit",    // the only thing allowed to refuse traffic
		"reticulum",    // off-Iridium bearer
		"oob",          // out-of-band commands
		"webhookroute", // tenant-addressed webhook paths
		"bridge",       // bridge MQTT ingest
		"quota",        // the ceiling itself must not learn about money
		"refunds",      // the credit-note outbox reads rows, never a provider
	}
	const why = "\nAn SOS is never affected by billing. This package can end a plan on a " +
		"webhook; it must not sit on a path that carries field traffic."

	for _, pkg := range ingest {
		dir := "../" + pkg

		out, err := exec.Command("go", "list", "-f", "{{join .Imports \"\\n\"}}", "./"+dir).Output()
		if err != nil {
			t.Fatalf("go list %s: %v", pkg, err)
		}
		for _, imp := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(imp) == self {
				t.Errorf("internal/%s imports internal/stripe."+why, pkg)
			}
		}

		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatalf("glob %s: %v", pkg, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			for _, forbidden := range []string{"ApplyStripeEvent(", "StripeCustomerID", "CheckoutSession("} {
				if strings.Contains(string(src), forbidden) {
					t.Errorf("%s references %s."+why, f, forbidden)
				}
			}
		}
	}
}

// TestStripeNeverTouchesDevices is the other half, and it matters more here
// than it did for refunds: this package writes to tenants on an inbound webhook
// from a third party. It may move a plan and record a payment. It may not reach
// a device, a bridge, a rate limiter or the quota.
func TestStripeNeverTouchesDevices(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	forbidden := []string{
		"DeleteDevice", "DeleteBridge", "UpdateDevice", "CreateDevice",
		"DeleteBridge(", "AllowAnother", "internal/quota", "internal/sos",
		"internal/ratelimit", "internal/bridge", "internal/routing",
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, bad := range forbidden {
			if strings.Contains(string(src), bad) {
				t.Errorf("%s references %s; this package moves a plan and records a payment, "+
					"and nothing else.", f, bad)
			}
		}
	}
	// The handler's store interface must stay narrow enough that it could not
	// touch a device even if somebody wanted it to.
	for _, f := range []string{"stripe.go", "webhook.go", "apply.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.Contains(string(src), "store.Store") {
			t.Errorf("%s takes the whole store; it should take only the slice it needs, "+
				"so it cannot reach a device row by accident.", f)
		}
	}
}

// The plan ceiling gates REGISTERING another device and nothing else. A
// cancellation lowers that ceiling; it must never be able to stop traffic, so
// the words that would do it are not allowed to appear here.
func TestEndingAPlanCannotSuspendATenant(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, bad := range []string{"StatusSuspended", "Status = \"suspended\"", "DeleteTenant"} {
			if strings.Contains(string(src), bad) {
				t.Errorf("%s references %s. A plan ending lowers a ceiling; it does not "+
					"suspend an account or stop a device reporting.", f, bad)
			}
		}
	}
}
