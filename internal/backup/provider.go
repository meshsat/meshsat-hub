package backup

import (
	"encoding/json"

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

func (p *HubStateProvider) ExportConfig() (json.RawMessage, error) {
	// Redact secrets before export
	redacted := p.Config
	redacted.RockBLOCKSecret = ""
	redacted.CloudloopAPIKey = ""
	redacted.AuthToken = ""
	redacted.APRSISPasscode = ""
	data, err := json.Marshal(redacted)
	return json.RawMessage(data), err
}

func (p *HubStateProvider) ExportWebhooks() (json.RawMessage, error) {
	if p.WebhookLister != nil {
		return p.WebhookLister.ListWebhooksRaw(), nil
	}
	return json.RawMessage("[]"), nil
}
