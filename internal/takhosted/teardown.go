package takhosted

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// PurgeAnnotation, set on a TakInstance before it is deleted, is what tells the
// operator to destroy the tenant's DATA along with the instance.
//
// Without it a delete stops the service and keeps everything: the CNPG Database
// and DatabaseRole carry a reclaim policy of retain, and only this marker
// switches them to delete. That asymmetry is deliberate -- "stop serving this
// tenant" and "destroy this customer's history" must never be the same gesture --
// and it means a teardown that forgets the marker reports success while the
// customer's TAK database lives on for good.
//
// The string is duplicated rather than imported. The operator owns the
// authoritative copy, in internal/takoperator's types.go, and this package is
// forbidden to import that one: it holds every tenant's CA key and the Hub
// terminates connections from the internet, a boundary enforced by
// TestTakhostedNeverReachesTheOperatorsCAMaterial. A duplicated constant can
// drift, so TestThePurgeAnnotationStillMatchesTheOperators reads the operator's
// source and fails when the two disagree.
const PurgeAnnotation = "tak.meshsat.net/purge-data"

// DeleteTAKInstanceForTenant destroys a tenant's hosted OpenTAKServer together
// with its database, and is how internal/tenancy's purge job satisfies the
// erasure promise for a closed account.
//
// It implements tenancy.TAKInstanceDeleter.
//
// Finding the instance by listing and matching spec.tenantID, rather than by
// reading the tak_instances row, is on purpose: the purge destroys that row, and
// this has to keep working on a retry after it is gone.
//
// A tenant with no instance is success, not an error. Most tenants never turn
// TAK on, and a second run after a partial failure must be able to finish.
func (c *Client) DeleteTAKInstanceForTenant(ctx context.Context, tenantID string) error {
	if tenantID == "" {
		return errors.New("takhosted: refusing to tear down a TAK instance for an empty tenant id")
	}

	instances, err := c.ListInstances(ctx)
	if err != nil {
		return fmt.Errorf("listing TAK instances for teardown: %w", err)
	}

	destroyed := 0
	for _, inst := range instances {
		if inst.Spec.TenantID != tenantID {
			continue
		}
		name := inst.Metadata.Name

		// Mark first, delete second, and never delete if the mark failed.
		//
		// Once the instance is gone there is nothing left to annotate, so the
		// data would be retained with no object left to ask for its removal. A
		// failure here is therefore reported to the caller, which leaves the
		// tenant's rows in place and tries again next run.
		if err := c.AnnotateInstance(ctx, name, map[string]string{PurgeAnnotation: "true"}); err != nil {
			if IsNotFound(err) {
				// Deleted underneath us between the list and now. Nothing to
				// destroy, and nothing to report.
				continue
			}
			return fmt.Errorf("marking a TAK instance for data destruction: %w", err)
		}
		if err := c.DeleteInstance(ctx, name); err != nil {
			return fmt.Errorf("deleting a marked TAK instance: %w", err)
		}
		destroyed++
		// Warn, not Info: this is irreversible and somebody reading the log after
		// the fact needs to find it. The instance name is left out deliberately,
		// since it identifies the tenant in a shared log.
		slog.Warn("takhosted: a tenant's TAK server was destroyed with its data",
			"tenant", tenantID)
	}

	if destroyed == 0 {
		slog.Debug("takhosted: no TAK instance to tear down", "tenant", tenantID)
	}
	return nil
}
