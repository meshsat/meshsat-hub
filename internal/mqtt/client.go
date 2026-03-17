package mqtt

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
)

// Client wraps the Paho MQTT client with auto-reconnect and health tracking.
type Client struct {
	inner     pahomqtt.Client
	connected atomic.Bool
	brokerURL string
}

// MessageHandler is called when a message is received on a subscribed topic.
type MessageHandler func(topic string, payload []byte)

// New creates a new MQTT client. Call Connect() to establish the connection.
func New(brokerURL, clientID string) *Client {
	c := &Client{brokerURL: brokerURL}

	opts := pahomqtt.NewClientOptions().
		AddBroker(brokerURL).
		SetClientID(clientID).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(30 * time.Second).
		SetKeepAlive(60 * time.Second).
		SetCleanSession(true).
		SetOnConnectHandler(func(_ pahomqtt.Client) {
			c.connected.Store(true)
			slog.Info("mqtt connected", "broker", brokerURL)
		}).
		SetConnectionLostHandler(func(_ pahomqtt.Client, err error) {
			c.connected.Store(false)
			slog.Warn("mqtt connection lost", "error", err)
		}).
		SetReconnectingHandler(func(_ pahomqtt.Client, _ *pahomqtt.ClientOptions) {
			slog.Info("mqtt reconnecting", "broker", brokerURL)
		})

	c.inner = pahomqtt.NewClient(opts)
	return c
}

// Connect establishes the connection to the MQTT broker.
func (c *Client) Connect() error {
	token := c.inner.Connect()
	token.Wait()
	if err := token.Error(); err != nil {
		return fmt.Errorf("mqtt connect to %s: %w", c.brokerURL, err)
	}
	return nil
}

// IsConnected returns true if the client is connected to the broker.
func (c *Client) IsConnected() bool {
	return c.connected.Load()
}

// Subscribe registers a handler for a topic pattern.
func (c *Client) Subscribe(topic string, qos byte, handler MessageHandler) error {
	token := c.inner.Subscribe(topic, qos, func(_ pahomqtt.Client, msg pahomqtt.Message) {
		handler(msg.Topic(), msg.Payload())
	})
	token.Wait()
	if err := token.Error(); err != nil {
		return fmt.Errorf("mqtt subscribe %s: %w", topic, err)
	}
	slog.Debug("mqtt subscribed", "topic", topic)
	return nil
}

// Publish sends a message to a topic.
func (c *Client) Publish(topic string, qos byte, retained bool, payload []byte) error {
	token := c.inner.Publish(topic, qos, retained, payload)
	token.Wait()
	if err := token.Error(); err != nil {
		return fmt.Errorf("mqtt publish %s: %w", topic, err)
	}
	return nil
}

// PublishJSON marshals v as JSON and publishes it.
func (c *Client) PublishJSON(topic string, qos byte, retained bool, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("mqtt marshal: %w", err)
	}
	return c.Publish(topic, qos, retained, data)
}

// Disconnect cleanly disconnects from the broker.
func (c *Client) Disconnect() {
	c.inner.Disconnect(1000)
	c.connected.Store(false)
}
