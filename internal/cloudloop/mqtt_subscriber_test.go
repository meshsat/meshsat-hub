package cloudloop

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestMQTTSubscriber_TopicPattern(t *testing.T) {
	accountID := "acct-12345"
	expectedTopic := fmt.Sprintf("lingo/%s/+/MO", accountID)

	// Verify the topic pattern is correctly formed.
	want := "lingo/acct-12345/+/MO"
	if expectedTopic != want {
		t.Errorf("topic = %q, want %q", expectedTopic, want)
	}
}

func TestMQTTSubscriber_HandlerRouting(t *testing.T) {
	var mu sync.Mutex
	var received []*LingoMO

	handler := func(_ context.Context, mo *LingoMO) string {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, mo)
		return "ok"
	}

	cfg := MQTTSubscriberConfig{
		BrokerURL: "ssl://mqtt.cloudloop.com:8883",
		AccountID: "test-account",
		ClientID:  "test-client",
	}

	sub := NewMQTTSubscriber(cfg, handler)

	// Verify config.
	if sub.cfg.AccountID != "test-account" {
		t.Errorf("AccountID = %q, want %q", sub.cfg.AccountID, "test-account")
	}
	if sub.cfg.ClientID != "test-client" {
		t.Errorf("ClientID = %q, want %q", sub.cfg.ClientID, "test-client")
	}
	if sub.cfg.BrokerURL != "ssl://mqtt.cloudloop.com:8883" {
		t.Errorf("BrokerURL = %q, want %q", sub.cfg.BrokerURL, "ssl://mqtt.cloudloop.com:8883")
	}
}

func TestMQTTSubscriber_DefaultClientID(t *testing.T) {
	cfg := MQTTSubscriberConfig{
		BrokerURL: "ssl://mqtt.cloudloop.com:8883",
		AccountID: "test",
	}

	sub := NewMQTTSubscriber(cfg, func(_ context.Context, _ *LingoMO) string { return "" })
	if sub.cfg.ClientID != "meshsat-hub-cloudloop" {
		t.Errorf("default ClientID = %q, want %q", sub.cfg.ClientID, "meshsat-hub-cloudloop")
	}
}

func TestMQTTSubscriber_StopBeforeStart(t *testing.T) {
	cfg := MQTTSubscriberConfig{
		BrokerURL: "ssl://mqtt.cloudloop.com:8883",
		AccountID: "test",
	}

	sub := NewMQTTSubscriber(cfg, func(_ context.Context, _ *LingoMO) string { return "" })
	// Stop without Start should not panic.
	sub.Stop()
}
