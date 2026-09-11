//go:build integration

package integration

import (
	"context"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
)

// platformTopics is the topic half of tenancy.Resolver.IsPlatformTopic, for
// tests that have no store: a device or bridge topic is the platform's when
// it names no customer tenant. The TAK and APRS-IS subscribers refuse to
// start without a checker (MESHSAT-1032).
type platformTopics struct{}

func (platformTopics) IsPlatformTopic(_ context.Context, topic string) bool {
	if tenant, _, _, ok := hubmqtt.ParseDeviceTopic(topic); ok {
		return tenant == hubmqtt.DefaultTenant
	}
	if tenant, _, _, ok := hubmqtt.ParseBridgeTopic(topic); ok {
		return tenant == hubmqtt.DefaultTenant
	}
	return false
}
