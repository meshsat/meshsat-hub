package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The tenant-scoping ratchet (MESHSAT-1149). See scoping.go for why.
//
// Two questions, both answered from source rather than from a running
// database, so they run on every push with no fixtures:
//
//  1. Does every Store method carry a tenant, or a written reason not to?
//  2. Does every SQL statement on a table with a tenant_id column filter by
//     it, or does the function that builds it have a written reason not to?

// scopedByArgument reports whether a method's parameter list carries a
// tenant: a parameter named tenantID/tenant, or a parameter whose type is a
// store struct with a TenantID field (the argument IS the row, tenant included).
func scopedByArgument(m *ast.FuncType, structsWithTenant map[string]bool) bool {
	for _, p := range m.Params.List {
		for _, n := range p.Names {
			switch n.Name {
			case "tenantID", "tenant", "tenantIDs":
				return true
			}
		}
		t := p.Type
		if star, ok := t.(*ast.StarExpr); ok {
			t = star.X
		}
		if arr, ok := t.(*ast.ArrayType); ok {
			t = arr.Elt
			if star, ok := t.(*ast.StarExpr); ok {
				t = star.X
			}
		}
		if id, ok := t.(*ast.Ident); ok && structsWithTenant[id.Name] {
			return true
		}
	}
	return false
}

// storeInterfaceMethods parses store.go and returns the Store interface's
// methods, plus the set of struct types in this package that carry TenantID.
func storeInterfaceMethods(t *testing.T) (map[string]*ast.FuncType, map[string]bool) {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	structsWithTenant := map[string]bool{}
	methods := map[string]*ast.FuncType{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.TYPE {
					continue
				}
				for _, s := range gd.Specs {
					ts := s.(*ast.TypeSpec)
					switch tt := ts.Type.(type) {
					case *ast.StructType:
						for _, fld := range tt.Fields.List {
							for _, n := range fld.Names {
								if n.Name == "TenantID" {
									structsWithTenant[ts.Name.Name] = true
								}
							}
						}
					case *ast.InterfaceType:
						if ts.Name.Name != "Store" {
							continue
						}
						for _, fld := range tt.Methods.List {
							ft, ok := fld.Type.(*ast.FuncType)
							if !ok {
								continue
							}
							for _, n := range fld.Names {
								methods[n.Name] = ft
							}
						}
					}
				}
			}
		}
	}
	if len(methods) == 0 {
		t.Fatal("could not find the Store interface in this package")
	}
	return methods, structsWithTenant
}

func TestEveryStoreMethodCarriesATenantOrAReason(t *testing.T) {
	methods, structsWithTenant := storeInterfaceMethods(t)

	var unscoped, unexplained []string
	for name, ft := range methods {
		if scopedByArgument(ft, structsWithTenant) || strings.Contains(name, "Tenant") {
			continue
		}
		unscoped = append(unscoped, name)
		if _, ok := UnscopedByDesign[name]; !ok {
			unexplained = append(unexplained, name)
		}
	}
	sort.Strings(unscoped)
	sort.Strings(unexplained)

	if len(unexplained) > 0 {
		t.Errorf("%d Store method(s) take no tenant and are not classified in UnscopedByDesign:\n  %s\n"+
			"Either give the method a tenantID parameter (the default for anything a customer's "+
			"data flows through) or add it to UnscopedByDesign in scoping.go with the reason it may "+
			"see every tenant. MESHSAT-1118 is what happens when this question is not asked.",
			len(unexplained), strings.Join(unexplained, "\n  "))
	}

	var stale []string
	for name := range UnscopedByDesign {
		ft, exists := methods[name]
		if !exists {
			stale = append(stale, name+" (no such method)")
			continue
		}
		if scopedByArgument(ft, structsWithTenant) || strings.Contains(name, "Tenant") {
			stale = append(stale, name+" (now takes a tenant; drop the entry)")
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("stale UnscopedByDesign entries:\n  %s", strings.Join(stale, "\n  "))
	}

	// The ratchet. Lower it when a method gains a tenant; never raise it
	// without the entry above AND a sentence in the commit explaining why.
	const auditedUnscoped = 54
	if len(unscoped) > auditedUnscoped {
		t.Errorf("Store has %d methods without a tenant, audited high-water mark is %d. "+
			"A new global method needs a reason in UnscopedByDesign and this constant raised in the same commit.",
			len(unscoped), auditedUnscoped)
	}
	if len(unscoped) < auditedUnscoped {
		t.Errorf("Store has %d methods without a tenant, fewer than the audited %d: lower auditedUnscoped so the ratchet keeps holding.",
			len(unscoped), auditedUnscoped)
	}
}

var (
	reCreateTable = regexp.MustCompile(`(?is)CREATE TABLE(?: IF NOT EXISTS)?\s+([a-z_]+)\s*\((.*)\)`)
	reAddTenant   = regexp.MustCompile(`(?i)ALTER TABLE\s+([a-z_]+)\s+ADD COLUMN(?: IF NOT EXISTS)?\s+tenant_id\b`)
	reTableRef    = regexp.MustCompile(`(?i)\b(?:FROM|UPDATE|INSERT(?: OR [A-Z]+)? INTO|JOIN|DELETE FROM)\s+([a-z_]+)\b`)
	reTenantCol   = regexp.MustCompile(`(?i)\btenant_id\b`)
)

// tenantTables reads the migration texts and returns every table that has a
// tenant_id column -- the same discovery the tenant export and purge do
// against the live catalogue, done here against the source.
func tenantTables(literals []string) map[string]bool {
	out := map[string]bool{}
	for _, lit := range literals {
		// One literal may hold several statements (postgres) or exactly one
		// (sqlite); split on the terminator so the greedy body match stays
		// inside its own CREATE TABLE.
		for _, stmt := range strings.Split(lit, ";") {
			m := reCreateTable.FindStringSubmatch(stmt)
			if m == nil {
				continue
			}
			if reTenantCol.MatchString(m[2]) {
				out[strings.ToLower(m[1])] = true
			}
		}
		for _, m := range reAddTenant.FindAllStringSubmatch(lit, -1) {
			out[strings.ToLower(m[1])] = true
		}
	}
	return out
}

type funcLiterals struct {
	pkg  string
	name string // the function name; both implementations share one entry
	text string // every string literal in the body, concatenated
}

// implementationLiterals parses one store implementation package and returns
// (a) every string literal at package level (the migrations) and (b) the
// string literals inside each function body, keyed by function.
func implementationLiterals(t *testing.T, dir, pkgName string) ([]string, []funcLiterals) {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var top []string
	var funcs []funcLiterals
	unquote := func(l *ast.BasicLit) string {
		s, err := strconv.Unquote(l.Value)
		if err != nil {
			return l.Value
		}
		return s
	}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				switch dd := d.(type) {
				case *ast.GenDecl:
					ast.Inspect(dd, func(n ast.Node) bool {
						if l, ok := n.(*ast.BasicLit); ok && l.Kind == token.STRING {
							top = append(top, unquote(l))
						}
						return true
					})
				case *ast.FuncDecl:
					if dd.Body == nil {
						continue
					}
					var sb strings.Builder
					ast.Inspect(dd.Body, func(n ast.Node) bool {
						if l, ok := n.(*ast.BasicLit); ok && l.Kind == token.STRING {
							sb.WriteString(unquote(l))
							sb.WriteString("\n")
						}
						return true
					})
					funcs = append(funcs, funcLiterals{pkg: pkgName, name: dd.Name.Name, text: sb.String()})
				}
			}
		}
	}
	return top, funcs
}

func TestEveryStatementOnATenantTableFiltersByTenant(t *testing.T) {
	impls := []struct{ dir, pkg string }{{"postgres", "postgres"}, {"sqlite", "sqlite"}}

	var violations, unfiltered []string
	seen := map[string]bool{}
	for _, impl := range impls {
		top, funcs := implementationLiterals(t, filepath.Join(".", impl.dir), impl.pkg)
		tables := tenantTables(top)
		if len(tables) < 10 {
			t.Fatalf("%s: found only %d tenant-scoped tables in the migrations; the parser is broken, not the schema", impl.pkg, len(tables))
		}
		for _, fn := range funcs {
			var touched []string
			for _, m := range reTableRef.FindAllStringSubmatch(fn.text, -1) {
				if tables[strings.ToLower(m[1])] {
					touched = append(touched, strings.ToLower(m[1]))
				}
			}
			if len(touched) == 0 || reTenantCol.MatchString(fn.text) {
				continue
			}
			sort.Strings(touched)
			unfiltered = append(unfiltered, fn.pkg+"."+fn.name)
			seen[fn.name] = true
			if _, ok := UnfilteredByDesign[fn.name]; !ok {
				violations = append(violations, fn.pkg+"."+fn.name+" touches "+strings.Join(uniq(touched), ",")+" without tenant_id")
			}
		}
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Errorf("%d store function(s) query a tenant-scoped table without filtering on tenant_id and are not "+
			"in UnfilteredByDesign:\n  %s\nAdd `AND tenant_id = $n` to the statement, or, if the function is "+
			"meant to see every tenant (a reaper, a drainer, a platform-admin listing), add it to "+
			"UnfilteredByDesign in scoping.go with the reason.",
			len(violations), strings.Join(violations, "\n  "))
	}

	var stale []string
	for name := range UnfilteredByDesign {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("stale UnfilteredByDesign entries (function gone or now filters by tenant; drop them):\n  %s",
			strings.Join(stale, "\n  "))
	}

	const auditedUnfiltered = 74
	if len(unfiltered) > auditedUnfiltered {
		t.Errorf("%d unfiltered tenant-table functions, audited high-water mark is %d", len(unfiltered), auditedUnfiltered)
	}
	if len(unfiltered) < auditedUnfiltered {
		t.Errorf("%d unfiltered tenant-table functions, fewer than the audited %d: lower auditedUnfiltered", len(unfiltered), auditedUnfiltered)
	}
}

func uniq(in []string) []string {
	var out []string
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}
