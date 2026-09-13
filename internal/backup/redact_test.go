package backup

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/config"
)

// MESHSAT-1116. ExportConfig redacted four fields by hand -- RockBLOCKSecret,
// CloudloopAPIKey, AuthToken, APRSISPasscode -- while config.Config grew to
// thirty-two secret-bearing fields around it. Verified against production: the
// export carried the LIVE Stripe secret key, the Stripe webhook signing secret
// and the Twilio API key SID, to any authenticated member of any tenant.
//
// A hand-maintained list of things to hide fails silently every time somebody
// adds a field and does not think about this file, so the rule is now structural.
// This test is the thing that keeps it structural.

// fill puts a recognisable value in every settable string field, so a field that
// escapes redaction shows up as its own name in the JSON rather than as "".
func fill(t *testing.T) config.Config {
	t.Helper()
	var cfg config.Config
	v := reflect.ValueOf(&cfg).Elem()
	ty := v.Type()
	for i := 0; i < ty.NumField(); i++ {
		if ty.Field(i).Type.Kind() == reflect.String && v.Field(i).CanSet() {
			v.Field(i).SetString("SENTINEL-" + ty.Field(i).Name)
		}
	}
	return cfg
}

// The one that would have caught the production leak.
func TestEverySecretLookingFieldIsRedacted(t *testing.T) {
	p := &HubStateProvider{Config: fill(t)}
	raw, err := p.ExportConfig()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	body := string(raw)

	ty := reflect.TypeOf(config.Config{})
	var leaked []string
	for i := 0; i < ty.NumField(); i++ {
		f := ty.Field(i)
		if f.Type.Kind() != reflect.String || !IsSecretFieldName(f.Name) {
			continue
		}
		if strings.Contains(body, "SENTINEL-"+f.Name) {
			leaked = append(leaked, f.Name)
		}
	}
	if len(leaked) > 0 {
		t.Errorf("%d secret-bearing fields survived redaction: %v\n"+
			"A backup is handed to a human and stored outside the cluster; a live key in it "+
			"is a disclosure, not an inconvenience.", len(leaked), leaked)
	}
}

// Name the ones that actually leaked, so the regression is specific and a
// reader of this test learns what was at stake rather than only the rule.
func TestTheFieldsThatLeakedInProductionAreRedacted(t *testing.T) {
	p := &HubStateProvider{Config: fill(t)}
	raw, _ := p.ExportConfig()
	body := string(raw)

	for _, name := range []string{
		"StripeSecretKey",     // charge cards, refund, read every payment
		"StripeWebhookSecret", // forge webhook deliveries
		"JWTSigningKey",       // forge any session
		"SMSAuthToken",        // send SMS on the platform account
		"AuthentikToken",      // identity provider admin
		"OIDCClientSecret",    // impersonate the Hub to the IdP
		"InvoiceNinjaToken",   // the billing system
		"MetricsToken",        // scrape internals
		"RockBLOCKSecret",     // these four were the original hand-written list;
		"CloudloopAPIKey",     // they must keep working, not be lost in the rewrite
		"AuthToken",           //
		"APRSISPasscode",      //
	} {
		if _, ok := reflect.TypeOf(config.Config{}).FieldByName(name); !ok {
			continue // renamed upstream; the structural test above still covers it
		}
		if strings.Contains(body, "SENTINEL-"+name) {
			t.Errorf("%s is present in the exported backup", name)
		}
	}
}

// Redaction must not corrupt the export: it still has to be the config, minus
// the secrets, and still parse.
func TestTheExportIsStillValidJSONAndKeepsNonSecrets(t *testing.T) {
	p := &HubStateProvider{Config: fill(t)}
	raw, err := p.ExportConfig()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("the redacted export is not valid JSON: %v", err)
	}
	// A non-secret string field must survive, or the redaction is too broad to
	// be useful and a restore would lose ordinary configuration.
	ty := reflect.TypeOf(config.Config{})
	kept := false
	for i := 0; i < ty.NumField(); i++ {
		f := ty.Field(i)
		if f.Type.Kind() == reflect.String && !IsSecretFieldName(f.Name) {
			if strings.Contains(string(raw), "SENTINEL-"+f.Name) {
				kept = true
				break
			}
		}
	}
	if !kept {
		t.Error("no non-secret string field survived; redaction is blanking everything")
	}
}
