package config

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The point of the whole exercise: a setting cannot enter this codebase without
// somebody deciding whose it is. Twenty-six settings that belonged to a tenant
// spent months as single global values because each one was added when nobody
// was asking, and the question was never revisited (MESHSAT-1121).
//
// This is deliberately a test and not a linter: it fails the build, in the same
// run as everything else, at the moment the field is added.
func TestEverySettingIsClassified(t *testing.T) {
	var missing []string
	seen := map[string]bool{}

	typ := reflect.TypeOf(Config{})
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("yaml")
		if tag == "" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		// yaml:"-" is a field deliberately kept out of the config FILE (a secret
		// that may only arrive by environment). It still has an owner, so it is
		// still classified -- keyed by its Go field name, which is all it has.
		if name == "-" {
			name = typ.Field(i).Name
		}
		seen[name] = true
		if _, ok := Classification[name]; !ok {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("%d config setting(s) have no owner in Classification: %s\n\n"+
			"Every setting belongs to somebody. Decide which:\n"+
			"  ClassPlatform       the deployment's own -- an env var is right\n"+
			"  ClassCommercial     a price or a plan ceiling -- the operator's lever\n"+
			"  ClassTenantDone     tenant-owned and already reachable in the UI\n"+
			"  ClassTenantProvider tenant-owned credentials -> internal/integrations\n"+
			"  ClassTenantColumn   tenant-owned policy -> a column on tenants\n\n"+
			"If it is tenant-owned, it does NOT ship as an environment variable only:\n"+
			"the owner's ruling is that all tenant configuration is reachable in the UI.",
			len(missing), strings.Join(missing, ", "))
	}

	// Drift the other way: a classification for a field that no longer exists is
	// a decision about nothing, and it makes the count above look complete when
	// it is not.
	var stale []string
	for name := range Classification {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Fatalf("Classification names %d setting(s) that no longer exist in Config: %s",
			len(stale), strings.Join(stale, ", "))
	}
}

// A tenant-owned setting must not be reachable ONLY through a file. This is the
// owner's ruling expressed as an assertion: anything classified as moving to the
// UI has to actually be moving, and the tranche that moves it deletes the env
// read. Until then the variable legitimately remains as the platform default,
// so this test asserts the weaker, checkable thing -- that we have not quietly
// grown NEW tenant-owned settings beyond the 25 the audit found.
//
// The number goes DOWN as tranches land (a setting becomes ClassTenantDone). It
// must never go up without a deliberate edit here, which is the conversation
// this test exists to force.
func TestNoNewTenantOwnedSettingsHaveAppeared(t *testing.T) {
	// A RATCHET, not a constant: it starts at the 22 the audit found and comes
	// down as each tranche lands, so a setting cannot quietly move back out of
	// the UI. APRS-IS took 4, Apprise and ntfy 5, the email gateway 6.
	const auditedProvider = 7 // hawkBit 4, WireGuard 3
	const auditedColumn = 3   // OOB max/hour, SMS timeout, satellite timeout

	provider, column := 0, 0
	for _, c := range Classification {
		switch c {
		case ClassTenantProvider:
			provider++
		case ClassTenantColumn:
			column++
		}
	}
	if provider > auditedProvider {
		t.Errorf("tenant-provider settings grew from %d to %d: a new tenant-owned "+
			"credential was added as a global value. It belongs in "+
			"internal/integrations, not in config.go.", auditedProvider, provider)
	}
	if column > auditedColumn {
		t.Errorf("tenant-column settings grew from %d to %d: a new tenant-owned "+
			"policy was added as a global value. It belongs on the tenants table "+
			"with platform bounds, the shape MESHSAT-1117 established.",
			auditedColumn, column)
	}
}

// HUB_OOB_ENCRYPT is gone and must stay gone. Sealing an out-of-band frame is
// not a preference: the frame commands real hardware -- reboot, power-cycle,
// factory reset -- over SMS and satellite bearers that are not confidential.
// The knob's unset position was "send in clear", and its only reachable effect
// was to disable sealing for every tenant at once from a ConfigMap (MESHSAT-1121).
func TestThereIsNoWayToTurnOffOOBEncryption(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if strings.Contains(name, "OOB") && strings.Contains(name, "Encrypt") {
			t.Fatalf("Config.%s exists again: OOB frames must always be sealed", name)
		}
	}

	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	// Not a bare Contains: the explanatory comment names the variable on purpose,
	// and a test that forbids naming it would delete the reason it is gone.
	if regexp.MustCompile(`os\.Getenv\("HUB_OOB_ENCRYPT"\)`).Match(src) {
		t.Fatal("config.go reads HUB_OOB_ENCRYPT again: OOB frames must always be sealed")
	}
}
