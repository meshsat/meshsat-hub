package auth

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

// routeTableFile is the single file every HTTP route is registered in.
const routeTableFile = "../../cmd/meshsat-hub/main.go"

type parsedRoute struct {
	method string // GET, POST, ... or ANY for Handle/HandleFunc
	path   string // as registered, chi placeholders intact
	gate   string // "", viewer, operator, owner, admin, platform-admin
}

func (r parsedRoute) key() string { return r.method + " " + r.path }

var routeMethods = map[string]string{
	"Get": "GET", "Post": "POST", "Put": "PUT", "Delete": "DELETE",
	"Patch": "PATCH", "Head": "HEAD", "Options": "OPTIONS",
	"Handle": "ANY", "HandleFunc": "ANY",
}

var mutating = map[string]bool{"POST": true, "PUT": true, "PATCH": true, "DELETE": true, "ANY": true}

// placeholder replaces chi path params so the real isExempt can judge a
// concrete path. isExempt is purely structural -- it counts segments and
// matches prefixes -- so any non-empty, dot-free token is faithful.
var placeholder = regexp.MustCompile(`\{[^}]*\}`)

func concretePath(p string) string { return placeholder.ReplaceAllString(p, "x") }

// gateIn reports the strongest RequireRole/RequirePlatformAdmin found anywhere
// in the given expression tree.
func gateIn(n ast.Node) string {
	best := ""
	ast.Inspect(n, func(x ast.Node) bool {
		ce, ok := x.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch f := ce.Fun.(type) {
		case *ast.SelectorExpr:
			name = f.Sel.Name
		case *ast.Ident:
			name = f.Name
		}
		found := ""
		switch name {
		case "RequirePlatformAdmin":
			found = "platform-admin"
		case "RequireRole":
			if len(ce.Args) == 1 {
				switch a := ce.Args[0].(type) {
				case *ast.SelectorExpr: // hubauth.RoleOwner
					found = strings.ToLower(strings.TrimPrefix(a.Sel.Name, "Role"))
				case *ast.BasicLit:
					if s, err := strconv.Unquote(a.Value); err == nil {
						found = s
					}
				}
			}
		}
		if RoleRank(found) > RoleRank(best) {
			best = found
		}
		return true
	})
	return best
}

// withGate extracts the gate from a `.With(...)` in the receiver chain of a
// route registration, e.g. r.With(RequireRole(RoleOwner)).Post(...).
func withGate(ce *ast.CallExpr) string {
	sel, ok := ce.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	inner, ok := sel.X.(*ast.CallExpr)
	if !ok {
		return ""
	}
	isel, ok := inner.Fun.(*ast.SelectorExpr)
	if !ok || isel.Sel.Name != "With" {
		return ""
	}
	best := ""
	for _, a := range inner.Args {
		if g := gateIn(a); RoleRank(g) > RoleRank(best) {
			best = g
		}
	}
	return best
}

func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	return s, err == nil
}

// collectRoutes walks a subtree carrying the accumulated r.Route prefix and
// the gate inherited from enclosing groups.
func collectRoutes(n ast.Node, prefix, gate string, out *[]parsedRoute) {
	ast.Inspect(n, func(x ast.Node) bool {
		ce, ok := x.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := ce.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		// A nested scope: r.Route("/p", func(r chi.Router){...}) or r.Group(fn).
		if name := sel.Sel.Name; name == "Route" || name == "Group" {
			var body *ast.FuncLit
			sub := prefix
			if name == "Group" && len(ce.Args) >= 1 {
				body, _ = ce.Args[0].(*ast.FuncLit)
			} else if len(ce.Args) >= 2 {
				if p, ok := stringLit(ce.Args[0]); ok {
					if p != "/" {
						sub = prefix + p
					}
					body, _ = ce.Args[1].(*ast.FuncLit)
				}
			}
			if body != nil {
				g := gate
				if wg := withGate(ce); RoleRank(wg) > RoleRank(g) {
					g = wg
				}
				// r.Use(...) at the top of the scope applies to all of it.
				for _, st := range body.Body.List {
					es, ok := st.(*ast.ExprStmt)
					if !ok {
						continue
					}
					uc, ok := es.X.(*ast.CallExpr)
					if !ok {
						continue
					}
					us, ok := uc.Fun.(*ast.SelectorExpr)
					if !ok || us.Sel.Name != "Use" {
						continue
					}
					for _, a := range uc.Args {
						if v := gateIn(a); RoleRank(v) > RoleRank(g) {
							g = v
						}
					}
				}
				collectRoutes(body, sub, g, out)
				return false // this subtree is accounted for
			}
		}

		m, isRoute := routeMethods[sel.Sel.Name]
		if !isRoute || len(ce.Args) < 1 {
			return true
		}
		p, ok := stringLit(ce.Args[0])
		if !ok || !strings.HasPrefix(p, "/") {
			return true
		}
		g := gate
		if wg := withGate(ce); RoleRank(wg) > RoleRank(g) {
			g = wg
		}
		full := prefix + p
		if p == "/" && prefix != "" {
			full = prefix
		}
		*out = append(*out, parsedRoute{method: m, path: full, gate: g})
		return true
	})
}

func parseRouteTable(t *testing.T) []parsedRoute {
	t.Helper()
	abs, err := filepath.Abs(routeTableFile)
	if err != nil {
		t.Fatalf("resolving %s: %v", routeTableFile, err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("cannot read the route table at %s: %v. "+
			"If routes moved out of cmd/meshsat-hub/main.go, point routeTableFile at the new file "+
			"-- do not delete this test, or the role floors stop being checked.", abs, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, abs, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", abs, err)
	}
	var routes []parsedRoute
	collectRoutes(f, "", "", &routes)
	sort.Slice(routes, func(i, j int) bool { return routes[i].key() < routes[j].key() })
	return routes
}

// TestParserSeesTheRouteTable is the guard that stops every other assertion in
// this file from passing vacuously. A parser that finds nothing reports a
// perfectly clean table.
func TestParserSeesTheRouteTable(t *testing.T) {
	routes := parseRouteTable(t)
	const minRoutes = 200
	if len(routes) < minRoutes {
		t.Fatalf("parsed only %d routes from %s, expected at least %d. "+
			"The parser is broken, not the route table: every assertion in this file is vacuous until it is fixed.",
			len(routes), routeTableFile, minRoutes)
	}
	var gated int
	for _, r := range routes {
		if r.gate != "" {
			gated++
		}
	}
	if gated == 0 {
		t.Fatalf("parsed %d routes and found no RequireRole/RequirePlatformAdmin gate on any of them, "+
			"which cannot be true. The gate detection is broken.", len(routes))
	}
}

// TestEveryWriteRequiresARole is the rule: changing state takes more than
// being signed in.
func TestEveryWriteRequiresARole(t *testing.T) {
	routes := parseRouteTable(t)

	var offenders []string
	for _, r := range routes {
		if !mutating[r.method] {
			continue
		}
		if isExempt(concretePath(r.path)) {
			continue // authenticates its own caller; see isExempt for each reason
		}
		if RoleRank(r.gate) >= RoleRank(RoleOperator) {
			continue
		}
		if _, ok := WritesWithoutRoleByDesign[r.key()]; ok {
			continue
		}
		offenders = append(offenders, r.key()+"  (gate: "+orNone(r.gate)+")")
	}
	if len(offenders) > 0 {
		t.Errorf("%d authenticated route(s) change state without requiring at least %q:\n  %s\n\n"+
			"Gate it with r.With(hubauth.RequireRole(hubauth.RoleOperator)) (or stronger), or add it to "+
			"WritesWithoutRoleByDesign in routefloor.go with the reason a viewer may do this.",
			len(offenders), RoleOperator, strings.Join(offenders, "\n  "))
	}

	// Two-sided ratchet on the exemptions themselves.
	const auditedViewerWrites = 0
	if len(WritesWithoutRoleByDesign) > auditedViewerWrites {
		t.Errorf("WritesWithoutRoleByDesign has %d entries, audited high-water mark is %d. "+
			"A new entry needs its reason AND this constant raised in the same commit.",
			len(WritesWithoutRoleByDesign), auditedViewerWrites)
	}
	if len(WritesWithoutRoleByDesign) < auditedViewerWrites {
		t.Errorf("WritesWithoutRoleByDesign has %d entries, fewer than the audited %d: "+
			"lower auditedViewerWrites so the ratchet keeps holding.",
			len(WritesWithoutRoleByDesign), auditedViewerWrites)
	}
}

// TestNamedRoutesKeepTheirFloor pins the routes whose floor comes from what
// they do rather than from their verb.
func TestNamedRoutesKeepTheirFloor(t *testing.T) {
	routes := parseRouteTable(t)
	byKey := make(map[string]parsedRoute, len(routes))
	for _, r := range routes {
		byKey[r.key()] = r
	}

	check := func(names []string, minGate, label string) {
		for _, want := range names {
			r, ok := byKey[want]
			if !ok {
				t.Errorf("%s names %q, which is not a registered route. "+
					"Either the route was renamed -- update routefloor.go in the same commit -- or the list is stale.",
					label, want)
				continue
			}
			if RoleRank(r.gate) < RoleRank(minGate) {
				t.Errorf("%s must require %s or stronger, has %q", want, minGate, orNone(r.gate))
			}
		}
	}
	check(MustRequireOwner, RoleOwner, "MustRequireOwner")
	check(MustRequirePlatformAdmin, "platform-admin", "MustRequirePlatformAdmin")
}

func orNone(g string) string {
	if g == "" {
		return "none"
	}
	return g
}
