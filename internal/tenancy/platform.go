package tenancy

import (
	"context"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
)

// IsPlatformTopic reports whether a device or bridge topic carries traffic
// owned by the platform (default) tenant.
//
// It is the one rule for every platform-level consumer that forwards device
// data to a system only the operator configured -- the TAK gateway, APRS-IS,
// TAK federation. A customer's positions, SOS events and messages must never
// reach those. [MESHSAT-1032]
//
// Both have to say platform:
//   - the topic: one that names a customer tenant is never platform traffic,
//     whoever the device belongs to. A customer bridge can only publish under
//     its own tenant's prefix (bridge.NATSPermissions), so this alone stops
//     everything a customer can send.
//   - the owner of record, exactly as for storage: a customer's registered
//     device on an untenanted topic (the Hub resolving a tenant wrongly) is
//     still the customer's.
//
// A topic that is neither a device nor a bridge topic is not platform traffic.
func (r *Resolver) IsPlatformTopic(ctx context.Context, topic string) bool {
	if tenant, device, _, ok := hubmqtt.ParseDeviceTopic(topic); ok {
		return tenant == hubmqtt.DefaultTenant && r.ForDeviceTopic(ctx, device, tenant) == r.def
	}
	if tenant, bridge, _, ok := hubmqtt.ParseBridgeTopic(topic); ok {
		return tenant == hubmqtt.DefaultTenant && r.ForBridgeTopic(ctx, bridge, tenant) == r.def
	}
	return false
}
