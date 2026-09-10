package refunds

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRefundsAreNotOnAnyIngestPath extends the SOS invariant to the billing
// work that arrived after it (MESHSAT-1019).
//
// The promise is unconditional: an SOS is never affected by billing. Not by a
// lapsed plan, not by being over the device ceiling, and not by a customer
// having asked for their money back. A refund shortens a paid period and
// produces a document; it must never sit anywhere near a path that carries
// field traffic, or a customer who withdrew from a subscription could lose a
// distress message.
//
// Like TestQuotaIsNotOnAnyIngestPath, this reads direct imports and source text
// rather than the transitive dependency graph, for the same reason: some of
// these packages reach internal/api for its JSON helpers and would drag the
// whole handler set in. A permanently red test is a deleted test.
func TestRefundsAreNotOnAnyIngestPath(t *testing.T) {
	const self = "github.com/meshsat/meshsat-hub/internal/refunds"
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
	}
	const why = "\nAn SOS is never affected by billing. A refund shortens a paid period; " +
		"it must not sit on a path that carries field traffic."

	for _, pkg := range ingest {
		dir := "../" + pkg

		out, err := exec.Command("go", "list", "-f", "{{join .Imports \"\\n\"}}", "./"+dir).Output()
		if err != nil {
			t.Fatalf("go list %s: %v", pkg, err)
		}
		for _, imp := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(imp) == self {
				t.Errorf("internal/%s imports internal/refunds."+why, pkg)
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
			for _, forbidden := range []string{"IssueCreditNote(", "CreateRefund(", "RefundedAt"} {
				if strings.Contains(string(src), forbidden) {
					t.Errorf("%s references %s."+why, f, forbidden)
				}
			}
		}
	}
}

// TestRefundsNeverTouchDevices is the other half. The drainer may move a plan's
// expiry date and nothing else: not a device row, not a bridge, not a rate
// limiter, not the quota. What a lapse means is the lapse job's single
// decision, and what a device may do is never a billing question.
func TestRefundsNeverTouchDevices(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	forbidden := []string{
		"DeleteDevice", "DeleteBridge", "UpdateDevice", "CreateDevice",
		"AllowAnother", "SetPlan(", "internal/quota", "internal/sos", "internal/ratelimit",
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
				t.Errorf("%s references %s; a refund moves an expiry date and nothing else.", f, bad)
			}
		}
	}
	// And the drainer's own store interface must stay narrow enough that it
	// could not touch a device even if somebody wanted it to.
	src, err := os.ReadFile("refunds.go")
	if err != nil {
		t.Fatalf("read refunds.go: %v", err)
	}
	if strings.Contains(string(src), "store.Store") {
		t.Error("the refund job takes the whole store; it should take only the slice it needs, " +
			"so it cannot reach a device row by accident.")
	}
}
