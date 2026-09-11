package aprsis

import "context"

// PlatformChecker answers whether a topic carries the platform tenant's
// traffic. *tenancy.Resolver implements it (IsPlatformTopic).
type PlatformChecker interface {
	IsPlatformTopic(ctx context.Context, topic string) bool
}

// platformOnly wraps an MQTT handler so it only ever sees the platform
// tenant's traffic. APRS-IS is a public network: a position injected there is
// published to the world under the operator's callsign, so a customer's
// device must never reach it. Without a checker nothing passes. [MESHSAT-1032]
func platformOnly(p PlatformChecker, handler func(string, []byte)) func(string, []byte) {
	return func(topic string, payload []byte) {
		if p == nil || !p.IsPlatformTopic(context.Background(), topic) {
			return
		}
		handler(topic, payload)
	}
}
