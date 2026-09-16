package auth

// Route authorisation floors, classified (MESHSAT-1189).
//
// Authentication and authorisation are separate questions, and this codebase
// answered the first one everywhere and the second one only where somebody
// remembered. The result was ~100 authenticated routes with no role gate at
// all, including POST /api/bridges/{id}/provision, which returns a bridge's
// plaintext MQTT password and its client PRIVATE KEY -- to any viewer of the
// tenant. MESHSAT-1171 had already found and fixed the same shape on the two
// routes either side of it (GenerateCredentials, IssueCertificate) and missed
// this one, which hands out strictly more. Fixing three routes by hand is not
// a fix for that; the class needs a mechanical question, the way tenant
// scoping got one in internal/store/scoping.go.
//
// routefloor_test.go parses the route table in cmd/meshsat-hub/main.go, works
// out the gate on each route from RequireRole / RequirePlatformAdmin -- whether
// applied with .With(), inherited from an enclosing r.Route group, or absent --
// and enforces two rules:
//
//  1. A route that MUTATES state (POST, PUT, PATCH, DELETE, or a bare Handle)
//     and is not auth-exempt must require at least OPERATOR. Read routes
//     default to viewer, which is what "authenticated" already means.
//
//  2. Every route named in MustRequireOwner must require owner or stronger,
//     and every route named in MustRequirePlatformAdmin must require the
//     platform axis. These are the routes where the specific act -- minting a
//     credential, moving key material, spending a tenant's airtime, reading
//     the whole process's memory -- is the reason, not the HTTP verb.
//
// An exemption from rule 1 goes in WritesWithoutRoleByDesign WITH ITS REASON,
// and the count in routefloor_test.go is a two-sided ratchet: it may fall when
// a route gains a gate, and raising it is a decision that has to be written
// down. The test also refuses to pass if it parses implausibly few routes,
// because a silently broken parser would report a clean table forever.

// WritesWithoutRoleByDesign lists every state-changing, authenticated route
// that legitimately requires no role beyond being signed in, with the reason.
// Keyed "METHOD /path" exactly as the route is registered.
var WritesWithoutRoleByDesign = map[string]string{
	// (empty) Every mutating authenticated route currently carries a gate.
	//
	// Keep it that way. If you are about to add an entry here, the question to
	// answer first is not "is this route harmless?" but "what can the least
	// privileged member of a tenant do with it?" -- which is the question that
	// was not asked about the provisioning bundle.
}

// MustRequireOwner names the routes where owner is the floor because of what
// the route DOES, independent of its verb. Matched exactly against
// "METHOD /path".
var MustRequireOwner = []string{
	// Mints, imports, moves or destroys credentials and key material.
	"POST /api/bridges/{id}/provision",
	"POST /api/bridges/{id}/provision/qr",
	"POST /api/bridges/{id}/credentials",
	"POST /api/bridges/{id}/certificate",
	"POST /api/bridges/{id}/credentials/rotate",
	"POST /api/devices/{imei}/keys",
	"POST /api/devices/{imei}/keys/import",
	"POST /api/devices/{imei}/keys/rotate",
	"POST /api/devices/{imei}/keys/distribute",
	"DELETE /api/devices/{imei}/keys/{id}",
	"POST /api/auth/keys",
	"POST /api/auth/keys/{id}/rotate",
	"DELETE /api/auth/keys/{id}",

	// Destroys a registration that a tenant's field hardware depends on, or
	// that resolves an inbound satellite IMEI to a tenant.
	"DELETE /api/bridges/{id}",
	"DELETE /api/devices/{imei}",

	// A standing spend decision: a routing rule turns inbound traffic into
	// outbound traffic on a bearer the tenant pays for, and internal/routing
	// has no rate limiter of its own.
	"POST /api/routes",
	"PUT /api/routes/{id}",
	"DELETE /api/routes/{id}",
}

// MustRequirePlatformAdmin names the routes that must sit on the platform
// axis rather than the tenant role axis. Matched exactly against "METHOD /path".
var MustRequirePlatformAdmin = []string{
	// A heap or goroutine dump of this process carries access tokens, refresh
	// tokens, webhook secrets and message plaintext for every tenant at once.
	// A tenant owner is not entitled to that, so role is the wrong axis.
	"ANY /debug/pprof/",
	"ANY /debug/pprof/cmdline",
	"ANY /debug/pprof/profile",
	"ANY /debug/pprof/symbol",
	"ANY /debug/pprof/trace",
	"ANY /debug/pprof/{profile}",

	// Cluster-wide, cross-tenant, or secret-bearing by construction.
	"POST /api/bridges/acl/regenerate",
	"POST /api/keys/channel/rotate",
	"GET /api/backup/export",
	"POST /api/backup/import",
}

// RoleRank ranks the gates a route can carry, strongest last. "" is no gate.
// PlatformAdmin is a separate axis from the tenant roles, but for the purpose
// of "is this route gated at least as strongly as X" it dominates, because a
// platform admin is strictly more privileged than any tenant role.
func RoleRank(gate string) int {
	switch gate {
	case RoleViewer:
		return 1
	case RoleOperator:
		return 2
	case RoleOwner, "admin":
		return 3
	case "platform-admin":
		return 4
	}
	return 0
}
