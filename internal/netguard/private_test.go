package netguard

import (
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// MESHSAT-1736: a self-hosted Hub on one organisation's network may reach the
// services its users configure there. The switch is the operator's, set once
// at boot; these tests set it and put it back.

func withPrivateTargets(t *testing.T, on bool) {
	t.Helper()
	prev := PrivateTargetsAllowed()
	AllowPrivateTargets(on)
	t.Cleanup(func() { AllowPrivateTargets(prev) })
}

func TestWithTheSwitchOnAnOperatorsOwnNetworkIsReachable(t *testing.T) {
	withPrivateTargets(t, true)
	for _, raw := range []string{
		"http://192.168.1.20:8008",
		"http://10.0.0.5",
		"http://127.0.0.1:8008",
		"http://localhost:8008",
		"http://172.16.2.2:8080",
		"http://100.64.0.1",
	} {
		if err := ValidatePublicURL(raw); err != nil {
			t.Errorf("ValidatePublicURL(%q) refused it with the switch on: %v", raw, err)
		}
	}
	// An internal name is no longer refused for its spelling; it is resolved
	// like any other, and a name nothing answers for stays refused (fail closed).
	for _, raw := range []string{"http://synapse.local:8008", "http://matrix.internal"} {
		err := ValidatePublicURL(raw)
		if err == nil || !strings.Contains(err.Error(), "cannot resolve") {
			t.Errorf("ValidatePublicURL(%q) with the switch on: %v, want a resolution failure", raw, err)
		}
	}
	if err := ValidatePublicHost("192.168.1.20"); err != nil {
		t.Errorf("ValidatePublicHost refused a LAN address with the switch on: %v", err)
	}
	for _, addr := range []string{"192.168.1.20:8008", "127.0.0.1:8008", "[::1]:8008", "10.1.2.3:443"} {
		if err := DialControl("tcp", addr, nil); err != nil {
			t.Errorf("DialControl(%q) refused it with the switch on: %v", addr, err)
		}
	}
}

func TestWhatStaysRefusedWithTheSwitchOn(t *testing.T) {
	withPrivateTargets(t, true)
	// Nothing a service is reached on: the cloud metadata service, multicast,
	// the unspecified address.
	for _, raw := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://0.0.0.0:80",
		"http://224.0.0.1",
		"file:///etc/passwd",
		"http://",
	} {
		if err := ValidatePublicURL(raw); err == nil {
			t.Errorf("ValidatePublicURL(%q) allowed it", raw)
		}
	}
	for _, addr := range []string{"169.254.169.254:80", "0.0.0.0:80", "224.0.0.1:80", "[ff02::1]:80", "[fe80::1]:80"} {
		if err := DialControl("tcp", addr, nil); err == nil {
			t.Errorf("DialControl(%q) allowed it", addr)
		}
	}
	// The pure classification does not move with the switch.
	if IsPublicIP(net.ParseIP("192.168.1.1")) {
		t.Error("IsPublicIP followed the switch")
	}
}

func TestTheSwitchOffIsTheOldRule(t *testing.T) {
	withPrivateTargets(t, false)
	for _, raw := range []string{"http://192.168.1.20:8008", "http://localhost:8008", "http://synapse.local"} {
		if err := ValidatePublicURL(raw); err == nil {
			t.Errorf("ValidatePublicURL(%q) allowed it with the switch off", raw)
		}
	}
	if err := ValidatePublicURL("http://synapse.local"); err == nil || !strings.Contains(err.Error(), "internal name") {
		t.Errorf("with the switch off an internal name is refused by its spelling, got %v", err)
	}
	if err := DialControl("tcp", "10.1.2.3:443", nil); err == nil {
		t.Error("DialControl allowed a private address with the switch off")
	}
}

// The hosted Hub serves tenants that are strangers to each other; the switch
// must never reach its manifests.
func TestTheHostedManifestsNeverTurnTheSwitchOn(t *testing.T) {
	root := filepath.Join("..", "..", "k8s")
	if _, err := os.Stat(root); err != nil {
		t.Skip("no k8s tree beside this package")
	}
	set := regexp.MustCompile(`HUB_ALLOW_PRIVATE_TARGETS\s*[:=]\s*"?(true|1|TRUE|True)"?`)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || (!strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml")) {
			return err
		}
		b, err := os.ReadFile(path) // #nosec G304 -- walking the repo's own manifests
		if err != nil {
			return err
		}
		if set.Match(b) {
			t.Errorf("%s turns HUB_ALLOW_PRIVATE_TARGETS on; the hosted Hub never may", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
