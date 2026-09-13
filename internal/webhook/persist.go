package webhook

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// The dispatcher's targets were in-memory only and nothing ever wrote them
// down (MESHSAT-1118). The store has carried tenant-scoped SaveWebhook,
// ListWebhooks and DeleteWebhook since the table was created, and webhook_configs
// has carried a tenant_id column with an index on it -- none of it was wired to
// anything. The API appended to a slice on one replica.
//
// So a customer's webhook survived until the next deploy and then vanished with
// no message, was invisible to the other replica, and came back from a backup
// export as somebody else's. This file is the missing half: the database is the
// record, memory is the hot-path cache of it, and a change is announced so both
// replicas agree.

// SetStore attaches persistence. A dispatcher with no store still works and is
// what the unit tests and a store-less deployment use; it simply forgets
// everything on restart, which is the behaviour this replaced.
func (d *Dispatcher) SetStore(s Store) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.store = s
}

func (d *Dispatcher) getStore() Store {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.store
}

// toStore and fromStore convert between this package's config and the store's.
// The two types differ only in how Events is typed, but they are separate on
// purpose: store.WebhookConfig carries CreatedAt, which the dispatcher has no
// use for, and EventType keeps the event vocabulary in one package.
func toStore(w WebhookConfig) store.WebhookConfig {
	events := make([]string, 0, len(w.Events))
	for _, e := range w.Events {
		events = append(events, string(e))
	}
	return store.WebhookConfig{
		ID: w.ID, URL: w.URL, Secret: w.Secret, Events: events,
		MaxRetries: w.MaxRetries, TimeoutSec: w.TimeoutSec, Enabled: w.Enabled,
	}
}

func fromStore(tenantID string, w store.WebhookConfig) WebhookConfig {
	events := make([]EventType, 0, len(w.Events))
	for _, e := range w.Events {
		events = append(events, EventType(e))
	}
	// withDefaults on the way out too, so a row written before the defaults were
	// persisted -- or by hand -- heals on load rather than delivering with no
	// timeout.
	return WebhookConfig{
		ID: w.ID, TenantID: tenantID, URL: w.URL, Secret: w.Secret, Events: events,
		MaxRetries: w.MaxRetries, TimeoutSec: w.TimeoutSec, Enabled: w.Enabled,
	}.withDefaults()
}

// LoadAll reads every tenant's webhooks into memory. Called once at startup.
//
// A tenant whose rows fail to load is logged and skipped rather than failing the
// whole load: one unreadable tenant must not leave every other tenant's webhooks
// silently unregistered.
func (d *Dispatcher) LoadAll(ctx context.Context) error {
	s := d.getStore()
	if s == nil {
		return nil
	}
	tenants, err := s.ListTenants(ctx)
	if err != nil {
		return err
	}
	var all []WebhookConfig
	for _, t := range tenants {
		rows, err := s.ListWebhooks(ctx, t.ID)
		if err != nil {
			slog.Error("webhook: loading a tenant's webhooks", "tenant", t.ID, "error", err)
			continue
		}
		for _, r := range rows {
			all = append(all, fromStore(t.ID, r))
		}
	}
	d.SetWebhooks(all)
	slog.Info("webhook: loaded from the database", "webhooks", len(all), "tenants", len(tenants))
	return nil
}

// ReloadTenant replaces one tenant's webhooks from the database, leaving every
// other tenant's alone.
func (d *Dispatcher) ReloadTenant(ctx context.Context, tenantID string) error {
	s := d.getStore()
	if s == nil || tenantID == "" {
		return nil
	}
	rows, err := s.ListWebhooks(ctx, tenantID)
	if err != nil {
		return err
	}
	fresh := make([]WebhookConfig, 0, len(rows))
	for _, r := range rows {
		fresh = append(fresh, fromStore(tenantID, r))
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	kept := make([]WebhookConfig, 0, len(d.webhooks)+len(fresh))
	for _, w := range d.webhooks {
		if w.TenantID != tenantID {
			kept = append(kept, w)
		}
	}
	d.webhooks = append(kept, fresh...)
	return nil
}

// Save persists one tenant's webhook and applies it here and on every other
// replica. The database write comes FIRST: a webhook that fires but is not
// recorded disappears at the next rollout, and the customer has no way to tell
// that from the Hub having forgotten it on purpose.
func (d *Dispatcher) Save(ctx context.Context, cfg WebhookConfig) error {
	cfg = cfg.withDefaults()
	if s := d.getStore(); s != nil {
		row := toStore(cfg)
		if err := s.SaveWebhook(ctx, cfg.TenantID, &row); err != nil {
			return err
		}
	}
	d.AddWebhook(cfg)
	d.announce(cfg.TenantID)
	return nil
}

// Delete removes one of the tenant's webhooks everywhere. It reports whether
// anything was removed.
func (d *Dispatcher) Delete(ctx context.Context, tenantID, id string) (bool, error) {
	removed := d.RemoveWebhook(tenantID, id)
	if s := d.getStore(); s != nil {
		// Issued even when memory held nothing: this replica may simply not have
		// caught up with a create made on the other one.
		if err := s.DeleteWebhook(ctx, tenantID, id); err != nil {
			return removed, err
		}
	}
	d.announce(tenantID)
	return removed, nil
}

// announce tells the other replicas to reload this tenant. A failure is logged
// and not returned: the change is already durable, and the other replica picks
// it up at its next restart. Refusing the customer's request at this point would
// be reporting a failure for work that succeeded.
func (d *Dispatcher) announce(tenantID string) {
	if d.mqtt == nil || tenantID == "" {
		return
	}
	payload, err := json.Marshal(reloadEvent{TenantID: tenantID})
	if err != nil {
		return
	}
	if err := d.mqtt.Publish(ReloadTopic, 1, false, payload); err != nil {
		slog.Warn("webhook: could not announce the change to other replicas",
			"tenant", tenantID, "error", err)
	}
}
