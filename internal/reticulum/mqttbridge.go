package reticulum

// MQTTBridge wraps an MQTT publisher that uses named handler types, adapting
// it to the MQTTPublisher interface expected by the Reticulum transport layer.
// This avoids importing the bus package directly (which would be circular).
type MQTTBridge struct {
	publishFn   func(topic string, qos byte, retained bool, payload []byte) error
	subscribeFn func(topic string, qos byte, handler func(string, []byte)) error
	connectedFn func() bool
}

// NewMQTTBridge creates an MQTT bridge from individual functions.
func NewMQTTBridge(
	publishFn func(string, byte, bool, []byte) error,
	subscribeFn func(string, byte, func(string, []byte)) error,
	connectedFn func() bool,
) *MQTTBridge {
	return &MQTTBridge{
		publishFn:   publishFn,
		subscribeFn: subscribeFn,
		connectedFn: connectedFn,
	}
}

func (b *MQTTBridge) Publish(topic string, qos byte, retained bool, payload []byte) error {
	return b.publishFn(topic, qos, retained, payload)
}

func (b *MQTTBridge) Subscribe(topic string, qos byte, handler func(string, []byte)) error {
	return b.subscribeFn(topic, qos, handler)
}

func (b *MQTTBridge) IsConnected() bool {
	return b.connectedFn()
}

// Compile-time check.
var _ MQTTPublisher = (*MQTTBridge)(nil)
