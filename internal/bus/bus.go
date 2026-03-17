// Package bus defines the message bus interface for MeshSat Hub.
// Implementations: paho (MQTT via Paho client) and natsbus (NATS JetStream).
// In standalone mode, NATS runs embedded in-process. In cluster/k8s mode,
// NATS runs as an external cluster. Both use the same natsbus implementation.
// Field devices always connect via MQTT — NATS speaks MQTT natively.
package bus

// MessageBus abstracts the pub/sub message layer.
// All Hub subscribers (TAK, APRS-IS, MT sender, webhooks) use this interface.
type MessageBus interface {
	// Connect establishes the connection to the bus.
	Connect() error

	// Publish sends a message to a topic.
	Publish(topic string, qos byte, retained bool, payload []byte) error

	// PublishJSON marshals v as JSON and publishes it.
	PublishJSON(topic string, qos byte, retained bool, v any) error

	// Subscribe registers a handler for a topic pattern.
	// In cluster mode, use QueueSubscribe for load-balanced consumption.
	Subscribe(topic string, qos byte, handler MessageHandler) error

	// QueueSubscribe registers a handler with a queue group.
	// Only one instance in the group receives each message.
	// Falls back to regular Subscribe if the bus doesn't support groups.
	QueueSubscribe(topic string, qos byte, group string, handler MessageHandler) error

	// IsConnected returns true if the bus is connected.
	IsConnected() bool

	// Disconnect cleanly closes the connection.
	Disconnect()
}

// MessageHandler is called when a message is received.
type MessageHandler func(topic string, payload []byte)
