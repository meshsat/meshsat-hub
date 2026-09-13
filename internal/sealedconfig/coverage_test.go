package sealedconfig

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every key that reaches system_config and is deliberately NOT sealed, with the
// reason. A key that is neither here nor in Sensitive fails the test below,
// which is the whole point: adding a secret to system_config should not be
// possible without someone deciding which list it belongs in.
var publicConfigKeys = map[string]string{
	"bridge_ca_cert":      "the CA certificate is a public trust anchor; NATS and every bridge need it",
	"mqtt_public_url":     "the broker URL the Fleet page hands out",
	"signup_notify_state": "when the operator was last told about pending signups",
}

// Prefixed keys. These hold credentials but are not sealed by this package,
// and each has its own answer recorded here rather than left to be rediscovered.
var prefixedConfigKeys = map[string]string{
	"provision_stash:": "one-time bridge bundle; the real fix is that it should not outlive its claim (MESHSAT-1098 sibling)",
	"provision_nonce:": "a nonce, not a secret at rest",
	"tak_enroll:":      "already AEAD-sealed under a nonce that is never stored, so a backup yields nothing openable",
}

// THE COVERAGE TEST. Sensitive is a hand-written list, so the failure mode is
// somebody storing the next private key in system_config and nobody noticing it
// went into the backups in clear text. This makes that a failing build.
//
// ⚠ IT COVERS KEYS WRITTEN AS STRING LITERALS AT THE CALL, WHICH TODAY IS THREE
// OF THEM. A package that names its key in a const -- internal/directory and
// internal/reticulum both do -- is invisible here, which is why
// TestTheNamedKeyConstantsStillMatchSensitive exists beside it. Between them
// the five are covered; neither test covers the five on its own, and saying so
// is cheaper than somebody later assuming this one did.
//
// Some entries in publicConfigKeys and prefixedConfigKeys are therefore not
// exercised by this test at all. They are kept because they record a decision
// that was made, and because a future refactor to literals would need them.
func TestEveryKeyThatReachesSystemConfigHasBeenClassified(t *testing.T) {
	root := filepath.Join("..", "..")
	call := regexp.MustCompile(`(?:Get|Set)SystemConfig\(\s*[A-Za-z_][A-Za-z0-9_.]*\s*,\s*"([a-z][a-z0-9_]*)"`)
	prefixed := regexp.MustCompile(`(?:Get|Set)SystemConfig\(\s*[A-Za-z_][A-Za-z0-9_.]*\s*,\s*"([a-z][a-z0-9_]*:)"\s*\+`)

	found := map[string][]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "dist", "vendor", "scratchpad":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path) // #nosec G304 -- test walking its own repo
		if rerr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		for _, m := range call.FindAllStringSubmatch(string(src), -1) {
			found[m[1]] = append(found[m[1]], rel)
		}
		for _, m := range prefixed.FindAllStringSubmatch(string(src), -1) {
			found[m[1]] = append(found[m[1]], rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("found no SystemConfig call sites at all -- the regex has drifted, so this " +
			"test is passing without checking anything")
	}

	var unclassified []string
	for key, files := range found {
		if IsSensitive(key) {
			continue
		}
		if _, ok := publicConfigKeys[key]; ok {
			continue
		}
		if _, ok := prefixedConfigKeys[key]; ok {
			continue
		}
		sort.Strings(files)
		unclassified = append(unclassified, key+"  ("+files[0]+")")
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Errorf("these keys reach system_config and are in neither Sensitive nor the "+
			"documented-public list:\n  %s\n\n"+
			"system_config is shipped off-site by barman with no encryption declared on the "+
			"data or the WAL. If the value is a secret, add it to Sensitive. If it is not, "+
			"add it to publicConfigKeys with the reason.",
			strings.Join(unclassified, "\n  "))
	}
}

// The named constants in internal/directory and internal/reticulum are what
// those packages actually pass, so a rename there would silently drop the key
// out of Sensitive while this package still compiled.
func TestTheNamedKeyConstantsStillMatchSensitive(t *testing.T) {
	for _, tc := range []struct{ file, want string }{
		{filepath.Join("..", "directory", "signing.go"), "directory_signing_key"},
		{filepath.Join("..", "reticulum", "hubidentity.go"), "reticulum_signing_key"},
		{filepath.Join("..", "reticulum", "hubidentity.go"), "reticulum_encryption_key"},
	} {
		src, err := os.ReadFile(tc.file) // #nosec G304 -- test reading its own repo
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		if !strings.Contains(string(src), `"`+tc.want+`"`) {
			t.Errorf("%s no longer contains %q, so Sensitive is sealing a key nothing uses "+
				"and the real one is going into the backups in clear text", tc.file, tc.want)
		}
		if !IsSensitive(tc.want) {
			t.Errorf("%q is used by %s but is not in Sensitive", tc.want, tc.file)
		}
	}
}
