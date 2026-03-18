// Package paho implements bus.MessageBus using Eclipse Paho MQTT client.
// Connects to any MQTT broker (Mosquitto, NATS MQTT adapter, etc.).
// Used in all modes — the broker differs, the client doesn't.
package paho

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/cubeos-app/meshsat-hub/internal/bus"
)

// Bus implements bus.MessageBus using Paho MQTT.
type Bus struct {
	brokerURL string
	clientID  string
	inner     pahomqtt.Client
	connected atomic.Bool
}

// New creates a new Paho MQTT bus.
func New(brokerURL, clientID string) *Bus {
	b := &Bus{brokerURL: brokerURL, clientID: clientID}

	opts := pahomqtt.NewClientOptions().
		AddBroker(brokerURL).
		SetClientID(clientID).
		SetProtocolVersion(4). // MQTT 3.1.1 — required by NATS MQTT adapter
		SetAutoReconnect(true).
		SetMaxReconnectInterval(30 * time.Second).
		SetKeepAlive(60 * time.Second).
		SetCleanSession(true).
		SetOnConnectHandler(func(_ pahomqtt.Client) {
			b.connected.Store(true)
			slog.Info("bus: mqtt connected", "broker", brokerURL)
		}).
		SetConnectionLostHandler(func(_ pahomqtt.Client, err error) {
			b.connected.Store(false)
			slog.Warn("bus: mqtt connection lost", "error", err)
		}).
		SetReconnectingHandler(func(_ pahomqtt.Client, _ *pahomqtt.ClientOptions) {
			slog.Info("bus: mqtt reconnecting", "broker", brokerURL)
		})

	b.inner = pahomqtt.NewClient(opts)
	return b
}

func (b *Bus) Connect() error {
	token := b.inner.Connect()
	token.Wait()
	if err := token.Error(); err != nil {
		return fmt.Errorf("bus: mqtt connect to %s: %w", b.brokerURL, err)
	}
	return nil
}

func (b *Bus) Publish(topic string, qos byte, retained bool, payload []byte) error {
	token := b.inner.Publish(topic, qos, retained, payload)
	token.Wait()
	if err := token.Error(); err != nil {
		return fmt.Errorf("bus: publish %s: %w", topic, err)
	}
	return nil
}

func (b *Bus) PublishJSON(topic string, qos byte, retained bool, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("bus: marshal: %w", err)
	}
	return b.Publish(topic, qos, retained, data)
}

func (b *Bus) Subscribe(topic string, qos byte, handler bus.MessageHandler) error {
	token := b.inner.Subscribe(topic, qos, func(_ pahomqtt.Client, msg pahomqtt.Message) {
		handler(msg.Topic(), msg.Payload())
	})
	token.Wait()
	if err := token.Error(); err != nil {
		return fmt.Errorf("bus: subscribe %s: %w", topic, err)
	}
	slog.Debug("bus: subscribed", "topic", topic)
	return nil
}

// QueueSubscribe falls back to regular Subscribe — Paho MQTT doesn't support
// shared subscriptions natively. For true queue groups, use MQTT 5.0 shared
// subscriptions ($share/group/topic) if the broker supports them.
func (b *Bus) QueueSubscribe(topic string, qos byte, group string, handler bus.MessageHandler) error {
	// MQTT 5.0 shared subscription format
	sharedTopic := fmt.Sprintf("$share/%s/%s", group, topic)
	err := b.Subscribe(sharedTopic, qos, handler)
	if err != nil {
		// Fall back to regular subscribe if broker doesn't support $share
		slog.Debug("bus: shared subscription failed, falling back to regular", "topic", topic, "group", group)
		return b.Subscribe(topic, qos, handler)
	}
	return nil
}

func (b *Bus) IsConnected() bool {
	return b.connected.Load()
}

func (b *Bus) Disconnect() {
	b.inner.Disconnect(1000)
	b.connected.Store(false)
}

// Compile-time check.
var _ bus.MessageBus = (*Bus)(nil)
