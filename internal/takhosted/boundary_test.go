package takhosted_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The security boundary this package is built around, enforced rather than
// merely intended.
//
// internal/takoperator holds every tenant's certificate authority. Its own
// package doc says why: "the Hub is internet-facing, and whoever holds a tenant's
// CA key can impersonate any phone in that tenant. So the key lives here, in a
// process with no public listener, and the Hub gets certificates by asking."
//
// That claim is only true while the Hub cannot reach the code. The operator
// exports NewTenantCA, LoadCA, ParseWrapKey and UnwrapKeyMaterial; an import
// would put all four within compile-time reach of the binary that terminates
// connections from the internet.
//
// Today nothing would be exploitable — the Hub has no wrap key, and its Role
// grants it nothing on the Secrets holding wrapped CA keys. That is a runtime
// accident. A boundary is something somebody can check, so this checks it.
//
// Same mechanism as internal/quota's TestQuotaIsNotOnAnyIngestPath, and for the
// same reason: direct imports AND source text, because the question is "can this
// package name that code", not "is it in the dependency graph somewhere".
func TestTakhostedNeverReachesTheOperatorsCAMaterial(t *testing.T) {
	const operator = "github.com/meshsat/meshsat-hub/internal/takoperator"
	const why = "\nThe operator holds every tenant's CA key. The Hub is internet-facing. " +
		"Importing that package puts NewTenantCA, LoadCA and UnwrapKeyMaterial within " +
		"compile-time reach of a process that accepts connections from strangers. " +
		"Duplicate the four small structs instead; that is the cheaper side of the trade."

	out, err := exec.Command("go", "list", "-f", "{{join .Imports \"\\n\"}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, imp := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(imp) == operator {
			t.Errorf("internal/takhosted imports internal/takoperator." + why)
		}
	}

	// And the test files too: a test that imports it would compile the link into
	// the test binary and, more importantly, would normalise the dependency.
	outT, err := exec.Command("go", "list", "-f", "{{join .TestImports \"\\n\"}}{{\"\\n\"}}{{join .XTestImports \"\\n\"}}", ".").Output()
	if err != nil {
		t.Fatalf("go list test imports: %v", err)
	}
	for _, imp := range strings.Split(string(outT), "\n") {
		if strings.TrimSpace(imp) == operator {
			t.Errorf("a test in internal/takhosted imports internal/takoperator." + why)
		}
	}

	// Source text: catches a dot-import, an alias, or somebody reaching for the
	// names before the import is even added.
	//
	// Test files are skipped, and that exclusion is what makes the check work
	// rather than a loophole. This file necessarily SAYS "NewTenantCA" and
	// "internal/takoperator." in prose, to explain the boundary; scanning it
	// would flag the explanation instead of a violation. Non-test files are
	// where a real use would live.
	//
	// The function names are matched with a trailing "(" so the package doc can
	// name them in prose, but the package qualifier is matched bare: any
	// occurrence of `takoperator.` in non-test source is a qualified identifier,
	// which is exactly the thing being forbidden. An earlier version of this test
	// appended "(" to the qualifier too, which looked for `takoperator.(` -- type
	// assertion syntax that never appears -- so that check was dead.
	callNames := []string{
		"NewTenantCA",
		"LoadCA",
		"UnwrapKeyMaterial",
		"WrapKeyMaterial",
		"ParseWrapKey",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found; this test would pass vacuously")
	}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		scanned++
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		text := string(src)
		for _, name := range callNames {
			if strings.Contains(text, name+"(") {
				t.Errorf("%s calls %s.%s", f, name, why)
			}
		}
		if strings.Contains(text, "takoperator.") {
			t.Errorf("%s uses a takoperator-qualified identifier.%s", f, why)
		}
	}
	if scanned == 0 {
		t.Fatal("every Go file here is a test file, so the source scan proved nothing")
	}
}

// The Hub must not be able to write a custom resource's status: that is the
// operator's half of the contract, and the release gate asserts the API server
// refuses it. This is the code-side half — nothing here should even try.
func TestTakhostedNeverWritesACustomResourceStatus(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.Contains(string(src), "/status") {
			t.Errorf("%s builds a /status subresource path.\nStatus is written only by the "+
				"operator; the Hub owns the spec. The Hub's Role grants no status verbs, so this "+
				"would fail with a 403 at runtime instead of at review.", f)
		}
	}
}
