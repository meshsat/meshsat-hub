package backup

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/meshsat/meshsat-hub/internal/config"
)

// WebhookLister is the interface for listing webhook configurations.
type WebhookLister interface {
	ListWebhooksRaw() json.RawMessage
}

// HubStateProvider implements StateProvider using Hub's config and webhook dispatcher.
type HubStateProvider struct {
	Config        config.Config
	WebhookLister WebhookLister
}

// secretFieldParts name the shapes a credential takes in config.Config. A field
// whose name contains any of them is blanked before export.
//
// This is a DENY-BY-DEFAULT rule on purpose (MESHSAT-1116). The previous version
// listed four fields by hand -- RockBLOCKSecret, CloudloopAPIKey, AuthToken,
// APRSISPasscode -- and the struct grew to thirty-two secret-bearing fields
// around it. The export was verified against production to carry the LIVE Stripe
// secret key, the Stripe webhook signing secret and the Twilio API key SID.
//
// A hand-maintained list of things to hide fails silently every time somebody
// adds a field and does not think about this file. Matching the shape of the
// name instead means a new secret is covered the day it is added, and
// TestEverySecretLookingFieldIsRedacted fails if the rule ever stops holding.
//
// Over-redaction is the safe direction: a restored backup missing an object-store
// key is an inconvenience, a leaked payment key is not.
var secretFieldParts = []string{
	"Secret", "Token", "Password", "Passcode", "Credential", "DSN", "Key",
}

// IsSecretFieldName reports whether a config field name looks like a credential.
// Exported so the test can walk config.Config with the same rule the code uses,
// rather than a second copy of the list that can drift from it.
func IsSecretFieldName(name string) bool {
	for _, part := range secretFieldParts {
		if strings.Contains(name, part) {
			return true
		}
	}
	return false
}

func (p *HubStateProvider) ExportConfig() (json.RawMessage, error) {
	redacted := p.Config
	v := reflect.ValueOf(&redacted).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		// Only strings, and only settable ones: an unexported field cannot be
		// written through reflection, and it also cannot be marshalled, so it
		// never reaches the export either way.
		if f.Type.Kind() != reflect.String || !v.Field(i).CanSet() {
			continue
		}
		if IsSecretFieldName(f.Name) {
			v.Field(i).SetString("")
		}
	}
	data, err := json.Marshal(redacted)
	return json.RawMessage(data), err
}

func (p *HubStateProvider) ExportWebhooks() (json.RawMessage, error) {
	if p.WebhookLister != nil {
		return p.WebhookLister.ListWebhooksRaw(), nil
	}
	return json.RawMessage("[]"), nil
}
