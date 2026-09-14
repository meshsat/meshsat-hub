package aprsis

import "context"

// TenantResolver answers which tenant owns the traffic on a topic.
// *tenancy.Resolver implements it (TenantForTopic).
//
// It replaced PlatformChecker in MESHSAT-1121. The old interface answered a
// yes/no question -- "is this the platform tenant's?" -- because there was one
// callsign and only the operator's own traffic could be allowed near it. That
// was containment for the leak MESHSAT-1032 found, not a decision that customers
// may not use APRS.
//
// Now each tenant transmits under its OWN licence, so the question became "whose
// is this?". The safety property is unchanged and comes from the resolver: the
// owner of record wins over the tenant named in the topic, so a publisher cannot
// move its traffic into another tenant, and therefore cannot borrow another
// tenant's callsign, by choosing a topic prefix.
type TenantResolver interface {
	TenantForTopic(ctx context.Context, topic string) string
}

// routeByTenant wraps an MQTT handler so it is told which tenant's traffic it
// received, and drops anything whose tenant cannot be established.
//
// Without a resolver nothing passes, exactly as platformOnly refused everything
// without a checker. Fail-closed is the only safe default here: the failure this
// guards is putting one person's location on a public network under another
// person's amateur licence.
func routeByTenant(r TenantResolver, handler func(tenantID, topic string, payload []byte)) func(string, []byte) {
	return func(topic string, payload []byte) {
		if r == nil {
			return
		}
		tenantID := r.TenantForTopic(context.Background(), topic)
		if tenantID == "" {
			return
		}
		handler(tenantID, topic, payload)
	}
}
