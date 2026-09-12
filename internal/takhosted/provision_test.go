package takhosted

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// This package may not import internal/takoperator -- it holds every tenant's CA
// key while the Hub is internet-facing (boundary_test.go enforces it) -- so the
// strings the two sides must agree on are duplicated. A duplicated string drifts
// silently, and each of these has a specific, invisible consequence when it does,
// so each is pinned by reading the operator's source as a FILE rather than
// importing it.

func operatorSource(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "takoperator", name)
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(src)
}

// squash removes whitespace, so a pin survives gofmt re-aligning a const block
// when somebody adds a longer name to it. A test that fails on reformatting gets
// deleted, and then the guard is gone -- so these compare VALUES, not layout.
func squash(s string) string {
	return strings.NewReplacer(" ", "", "\t", "").Replace(s)
}

// The object name. If the Hub declares tak-<label> and the operator looks for
// something else, the operator never sees the instance, and a customer waits
// forever for a server nothing is building.
func TestTheInstanceObjectNameMatchesTheOperators(t *testing.T) {
	src := squash(operatorSource(t, "types.go"))
	const want = `funcInstanceName(labelstring)string{return"tak-"+label}`
	if !strings.Contains(src, want) {
		t.Errorf("internal/takoperator no longer builds an instance name as \"tak-\" + label.\n" +
			"This package builds it itself (instanceObjectName) because it may not import that " +
			"one. If the two disagree the Hub declares instances the operator never finds, and a " +
			"customer waits for a server nothing is building.")
	}
	if got := instanceObjectName("abcdefghij"); got != "tak-abcdefghij" {
		t.Errorf("instanceObjectName = %q, want tak-abcdefghij", got)
	}
}

// The state and phase literals. A state outside the CRD's enum is refused at
// admission; a phase that is not one the operator reports makes the Hub's cached
// row disagree with the instance it describes.
func TestTheStateAndPhaseLiteralsMatchTheOperators(t *testing.T) {
	src := squash(operatorSource(t, "types.go"))
	for _, pin := range []struct{ name, mine, what string }{
		{"StateRunning", stateRunning, "the state a new instance asks for"},
		{"PhaseProvisioning", phaseProvisioning, "the phase recorded until the operator reports one"},
	} {
		want := pin.name + `="` + pin.mine + `"`
		if !strings.Contains(src, want) {
			t.Errorf("the operator no longer declares %s = %q (%s); this package's copy has drifted, "+
				"and nothing else would notice", pin.name, pin.mine, pin.what)
		}
	}
}

// A minted label satisfies the CRD's pattern by construction, because it becomes
// a Postgres database name (tak_<label>) and part of a CA subject as well as an
// object name. Hex, not base64url: the latter yields uppercase, '-' and '_', none
// of which the pattern accepts.
func TestAMintedLabelSatisfiesTheCRDPattern(t *testing.T) {
	pattern := regexp.MustCompile(`^[a-z0-9]{10}$`)

	// Prove the arithmetic rather than trusting it.
	if labelBytes*2 != 10 {
		t.Fatalf("labelBytes=%d hex-encodes to %d characters, want 10", labelBytes, labelBytes*2)
	}

	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		label, err := mintLabelValue()
		if err != nil {
			t.Fatalf("minting: %v", err)
		}
		if !pattern.MatchString(label) {
			t.Fatalf("minted %q, which the CRD pattern refuses", label)
		}
		seen[label] = true
	}
	if len(seen) < 190 {
		t.Errorf("only %d distinct labels in 200 draws; a value that must be unique across "+
			"tenants is not random enough", len(seen))
	}
}

// The CRD is the authority on that pattern, so confirm it still says what the
// minter assumes.
func TestTheCRDStillConstrainsTheLabelToTenLowercaseAlphanumerics(t *testing.T) {
	path := filepath.Join("..", "..", "k8s", "tak-operator", "crds", "takinstance.yaml")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(src), `pattern: '^[a-z0-9]{10}$'`) {
		t.Errorf("%s no longer constrains the label to ^[a-z0-9]{10}$, which is what the minter "+
			"in this package produces ten hex characters on the strength of", path)
	}
}

// A label is never derived from anything identifying. It reaches the CA subject,
// which travels to every phone in the tenant, and object names anyone with
// namespace access can list.
func TestAMintedLabelCarriesNothingFromTheTenant(t *testing.T) {
	// mintLabelValue takes no arguments at all, which is the structural guarantee.
	// This test exists to state why that signature must not grow one.
	label, err := mintLabelValue()
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	for _, identifying := range []string{"tenant", "acme", "t1", "default"} {
		if strings.Contains(label, identifying) {
			t.Errorf("minted label %q contains %q; a label must say nothing about the customer, "+
				"because it appears in the CA subject every phone receives", label, identifying)
		}
	}
}
