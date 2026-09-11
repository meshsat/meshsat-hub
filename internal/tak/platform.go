package tak

import "context"

// PlatformChecker answers whether a topic carries the platform tenant's
// traffic. *tenancy.Resolver implements it (IsPlatformTopic).
type PlatformChecker interface {
	IsPlatformTopic(ctx context.Context, topic string) bool
}

// platformOnly wraps an MQTT handler so it only ever sees the platform
// tenant's traffic. The TAK server, like any federation peer, is the
// operator's: every ATAK client on it sees what arrives, so a customer's
// positions, SOS events and messages must never be sent there. Without a
// checker nothing passes. [MESHSAT-1032]
func platformOnly(p PlatformChecker, handler func(string, []byte)) func(string, []byte) {
	return func(topic string, payload []byte) {
		if p == nil || !p.IsPlatformTopic(context.Background(), topic) {
			return
		}
		handler(topic, payload)
	}
}

// SetPlatformChecker sets the rule deciding which traffic may be federated.
// Until it is set, nothing is. [MESHSAT-1032]
func (f *Federation) SetPlatformChecker(p PlatformChecker) { f.platform = p }
