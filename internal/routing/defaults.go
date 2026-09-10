package routing

import (
	"context"
	"log/slog"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// DefaultRoutes returns the set of default fanout routes for a new tenant.
// These ensure satellite messages reach all enabled channels without configuration.
// Source "satellite" (iridium, iridium_imt, globalstar): an SMS or email that
// enters the routing engine is not fanned out to TAK, APRS, webhooks and
// notifications by default (MESHSAT-1022).
func DefaultRoutes() []store.Route {
	return []store.Route{
		{Name: "Satellite → TAK", SourceType: "satellite", DestinationType: "tak", Enabled: true},
		{Name: "Satellite → APRS", SourceType: "satellite", DestinationType: "aprs", Enabled: true},
		{Name: "Satellite → Webhooks", SourceType: "satellite", DestinationType: "webhook", Enabled: true},
		{Name: "Satellite → Notifications", SourceType: "satellite", DestinationType: "notification", Enabled: true},
		{Name: "Satellite → MQTT Fanout", SourceType: "satellite", DestinationType: "mqtt", Enabled: true},
	}
}

// SeedDefaults creates default routes for a tenant if none exist.
func SeedDefaults(ctx context.Context, s store.Store, tenantID string) error {
	existing, err := s.ListRoutes(ctx, tenantID)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return nil // tenant already has routes
	}

	defaults := DefaultRoutes()
	for i := range defaults {
		if err := s.CreateRoute(ctx, tenantID, &defaults[i]); err != nil {
			slog.Warn("routing: failed to seed default route", "name", defaults[i].Name, "error", err)
		}
	}
	slog.Info("routing: seeded default routes", "tenant", tenantID, "count", len(defaults))
	return nil
}
