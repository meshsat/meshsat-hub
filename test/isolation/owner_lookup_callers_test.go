package isolation

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ownerLookupCallers are the only places allowed the lookup in which the
// registered owner beats the namespace. Everything else reads the bus and has
// to use AdmitDeviceTopic or AdmitBridgeTopic (MESHSAT-1469).
var ownerLookupCallers = map[string]string{
	"internal/cloudloop/webhook.go":  "provider delivery: no tenant of its own to believe",
	"internal/rockblock/handler.go":  "provider delivery: no tenant of its own to believe",
	"internal/globalstar/handler.go": "provider delivery: no tenant of its own to believe",
	"internal/tenancy/platform.go":   "IsPlatformTopic: both the topic and the owner have to say platform",
	"internal/personchat/service.go": "compares the answer to the topic's tenant and refuses a difference",
	"internal/bridge/uplink.go":      "compares the answer to the tenant and refuses a difference",
	"internal/bridge/subscriber.go":  "the bridge's own subtree, which the broker confines to its own id",
}

// A consumer quietly written with the old lookup reopens the hole with nothing
// to show for it: everything works, and a forgery is filed under its victim.
func TestOnlyCallersWithNoTenantToBelieveUseTheOwnerLookup(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "web" || d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "ForDeviceTopic" && sel.Sel.Name != "ForBridgeTopic") {
					return true
				}
				if _, allowed := ownerLookupCallers[rel]; !allowed {
					t.Errorf("%s calls %s: a consumer of the bus has to use the Admit methods", fset.Position(call.Pos()), sel.Sel.Name)
				}
				seen[rel] = true
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for file := range ownerLookupCallers {
		if !seen[file] {
			t.Errorf("%s no longer uses the owner lookup: remove it from the list", file)
		}
	}
}
