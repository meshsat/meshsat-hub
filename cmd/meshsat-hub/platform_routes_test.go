package main

import (
	"os"
	"strings"
	"testing"
)

// MESHSAT-1116. These routes read or write state that belongs to the PLATFORM,
// not to a tenant: the host kernel, a single WireGuard server, one hawkBit
// instance, a process-wide PGP keyring, the tenant-less system_config table,
// the cluster's NATS auth Secret, and a backup of the Hub's own configuration.
//
// Every one of them was registered at the router root behind nothing but
// authentication, so any member of any tenant -- a viewer included -- could
// reach them. The backup export was verified against production to hand out the
// live Stripe secret key.
//
// The structural cause is worth stating, because it is why RoleOwner is not the
// fix: auth.RequireRole reads only user.Roles and never consults the tenant
// (internal/auth/rbac.go), so RequireRole(RoleOwner) is satisfied by the owner
// of ANY tenant. auth.RequirePlatformAdmin checks a separate boolean and is the
// only platform gate there is.
//
// This test reads main.go rather than exercising the router, for the same reason
// internal/quota pins its invariant by scanning source: the property is "nobody
// moved this route out of the gate", and that is a property of the registration
// site, not of a response.
var platformOnlyRoutes = []string{
	// Writes system_config, which has no tenant_id: one value for every tenant.
	`"/api/settings/mqtt-url"`,
	// Discloses platform service-auth status and the secrets file path.
	`"/api/settings/security"`,
	// Rewrites the deployment's service passwords on disk.
	`"/api/settings/security/rotate"`,
	// The Hub's own configuration, and every tenant's data.
	`"/api/backup/export"`,
	`"/api/backup/diff"`,
	`"/api/backup/import"`,
	// Rekeys and broadcasts to every online bridge of the default tenant.
	`"/api/keys/channel/rotate"`,
	// Re-renders the cluster-wide meshsat-nats-auth Secret.
	`"/api/bridges/acl/regenerate"`,
	// Host kernel path-manager state, via `ip mptcp`.
	`"/api/mptcp/status"`,
	`"/api/mptcp/strategy"`,
	`"/api/mptcp/endpoints"`,
	`"/api/mptcp/endpoints/{id}"`,
	// The three /api/wireguard/peers routes came off this list in MESHSAT-1121,
	// for the same reason the email key routes did: the platform-admin gate was
	// containment for a defect, not a property of the endpoint.
	//
	// There was ONE wg-easy for the whole deployment and the handler had no
	// tenant model, so any member could enumerate, create and delete another
	// tenant's peers -- and the config download carries a peer's PRIVATE KEY.
	// Each tenant now brings its own server (internal/wireguard.ClientPool) and
	// the provisioner's peer map is keyed by tenant AND device, so the endpoints
	// reach only the caller's own peers and are owner-gated like the rest of a
	// tenant's settings.
	//
	// If a shared server or a device-keyed peer map ever returns, put them BACK.
	// Firmware to the fleet. The highest-impact primitive on the router.
	`"/api/ota/targets"`,
	`"/api/ota/targets/{controllerId}"`,
	`"/api/ota/targets/{controllerId}/actions"`,
	`"/api/ota/targets/{controllerId}/actions/{actionId}"`,
	`"/api/ota/rollouts"`,
	`"/api/ota/rollouts/{id}"`,
	`"/api/ota/rollouts/{id}/start"`,
	`"/api/ota/rollouts/{id}/pause"`,
	// /api/email/keys and /api/email/keys/{email} were here until MESHSAT-1121,
	// and came off the list deliberately rather than by accident.
	//
	// The reason they were platform-only was a DEFECT, not a property of the
	// endpoint: the PGP keyring was one process-wide map keyed by bare email
	// address, so listing showed every tenant's correspondents and overwriting an
	// address's public key redirected that recipient's encrypted mail to a key
	// somebody else supplied. Gating to platform admins contained it; it did not
	// fix it, and it left customers unable to manage their own correspondents.
	//
	// The keyring is per tenant now (internal/email.Pool), so the endpoints reach
	// only the caller's own contacts and are owner-gated like the rest of a
	// tenant's settings. internal/email.TestTwoTenantsContactsForTheSameAddressDoNotCollide
	// is what holds that, and it is mutation-tested against a shared keyring.
	//
	// If a future change reintroduces a shared keyring, put them BACK here.
}

// gatedLines returns, for every line of main.go, whether a route registered on
// that line sits inside a RequirePlatformAdmin scope.
//
// It tracks brace depth so an `r.Group`/`r.Route` body that calls
// RequirePlatformAdmin marks every line within it, and handles the inline
// `r.With(hubauth.RequirePlatformAdmin()).Post(...)` form on the line itself.
func gatedLines(t *testing.T, src string) []bool {
	t.Helper()
	lines := strings.Split(src, "\n")
	out := make([]bool, len(lines))

	// depth -> whether the scope opened at that depth is gated.
	gatedAt := map[int]bool{}
	depth := 0
	for i, ln := range lines {
		code := ln
		if idx := strings.Index(code, "//"); idx >= 0 {
			code = code[:idx] // crude, but main.go has no // inside a route string
		}
		opens := strings.Count(code, "{")
		closes := strings.Count(code, "}")

		if strings.Contains(code, "RequirePlatformAdmin()") && strings.Contains(code, "r.Use(") {
			gatedAt[depth] = true
		}
		inScope := false
		for d := 0; d <= depth; d++ {
			if gatedAt[d] {
				inScope = true
				break
			}
		}
		out[i] = inScope || strings.Contains(code, "r.With(hubauth.RequirePlatformAdmin())")

		depth += opens - closes
		if closes > opens {
			// Scopes closed on this line are no longer gated.
			for d := depth + 1; d <= depth+(closes-opens); d++ {
				delete(gatedAt, d)
			}
		}
	}
	return out
}

func TestEveryPlatformOnlyRouteIsBehindPlatformAdmin(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(raw)
	gated := gatedLines(t, src)
	lines := strings.Split(src, "\n")

	// Sanity: the scanner must actually find gated lines, or this test would
	// pass by finding nothing and prove nothing.
	any := false
	for _, g := range gated {
		if g {
			any = true
			break
		}
	}
	if !any {
		t.Fatal("the scanner found no RequirePlatformAdmin scopes at all; it is broken, " +
			"and a broken scanner here reads as a clean bill of health")
	}

	for _, route := range platformOnlyRoutes {
		found, ungated := false, []int{}
		for i, ln := range lines {
			// A registration line, not a comment mentioning the path.
			if !strings.Contains(ln, route) {
				continue
			}
			// Match on the method call alone, not "r.Get(": the inline form is
			// r.With(hubauth.RequirePlatformAdmin()).Post("/…"), where the
			// receiver is the With() result. Requiring "r.Post(" silently
			// skipped exactly the routes that WERE gated that way — which this
			// test caught on its first run, and which is the failure mode a
			// source-scanning test has to be written against.
			if !strings.Contains(ln, ".Get(") && !strings.Contains(ln, ".Post(") &&
				!strings.Contains(ln, ".Put(") && !strings.Contains(ln, ".Delete(") &&
				!strings.Contains(ln, ".Patch(") {
				continue
			}
			found = true
			if !gated[i] {
				ungated = append(ungated, i+1)
			}
		}
		if !found {
			t.Errorf("%s is no longer registered in main.go. If it was removed, drop it from "+
				"platformOnlyRoutes deliberately; if it was renamed, this list must follow it.", route)
			continue
		}
		if len(ungated) > 0 {
			t.Errorf("%s is registered OUTSIDE a RequirePlatformAdmin scope at line(s) %v.\n"+
				"This route reaches platform-global state. Ungated, any authenticated member of "+
				"any tenant can call it.", route, ungated)
		}
	}
}

// RoleOwner is not a substitute, and the reason is easy to forget.
func TestPlatformRoutesAreNotMerelyRoleGated(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	for _, route := range platformOnlyRoutes {
		for i, ln := range lines {
			if !strings.Contains(ln, route) || !strings.Contains(ln, "hubauth.RequireRole(") {
				continue
			}
			t.Errorf("line %d gates %s with RequireRole. RequireRole reads only user.Roles and "+
				"never consults the tenant, so the owner of ANY tenant satisfies it. Platform "+
				"state needs RequirePlatformAdmin.", i+1, route)
		}
	}
}
