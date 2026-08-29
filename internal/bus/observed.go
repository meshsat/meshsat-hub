package bus

import (
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/observability"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	mqttPublishedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_mqtt_messages_published_total",
		Help: "Total MQTT messages published by topic pattern.",
	}, []string{"topic_pattern"})

	mqttReceivedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "meshsat_hub_mqtt_messages_received_total",
		Help: "Total MQTT messages received by topic pattern.",
	}, []string{"topic_pattern"})

	mqttPublishDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "meshsat_hub_mqtt_publish_duration_seconds",
		Help:    "Duration of MQTT publish operations in seconds.",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1},
	})

	mqttConnected = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshsat_hub_mqtt_connected",
		Help: "Whether the MQTT broker is connected (1=yes, 0=no).",
	})

	mqttReconnectsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "meshsat_hub_mqtt_reconnects_total",
		Help: "Total MQTT reconnection attempts.",
	})

	mqttSubscriptionsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "meshsat_hub_mqtt_subscriptions_active",
		Help: "Number of active MQTT subscriptions.",
	})
)

// ObservedBus wraps a MessageBus with Prometheus metrics.
type ObservedBus struct {
	inner        MessageBus
	wasConnected bool
}

// NewObservedBus wraps a MessageBus with instrumentation.
func NewObservedBus(inner MessageBus) *ObservedBus {
	return &ObservedBus{inner: inner}
}

func (o *ObservedBus) Connect() error {
	err := o.inner.Connect()
	if err != nil {
		observability.RecordError(observability.ErrMQTT, "bus")
	} else {
		mqttConnected.Set(1)
		o.wasConnected = true
	}
	return err
}

func (o *ObservedBus) Publish(topic string, qos byte, retained bool, payload []byte) error {
	start := time.Now()
	err := o.inner.Publish(topic, qos, retained, payload)
	mqttPublishDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		observability.RecordError(observability.ErrMQTT, "bus")
	} else {
		mqttPublishedTotal.WithLabelValues(normalizeTopic(topic)).Inc()
	}
	return err
}

func (o *ObservedBus) PublishJSON(topic string, qos byte, retained bool, v any) error {
	start := time.Now()
	err := o.inner.PublishJSON(topic, qos, retained, v)
	mqttPublishDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		observability.RecordError(observability.ErrMQTT, "bus")
	} else {
		mqttPublishedTotal.WithLabelValues(normalizeTopic(topic)).Inc()
	}
	return err
}

func (o *ObservedBus) Subscribe(topic string, qos byte, handler MessageHandler) error {
	wrapped := func(t string, payload []byte) {
		mqttReceivedTotal.WithLabelValues(normalizeTopic(t)).Inc()
		handler(t, payload)
	}
	err := o.inner.Subscribe(topic, qos, wrapped)
	if err != nil {
		observability.RecordError(observability.ErrMQTT, "bus")
	} else {
		mqttSubscriptionsActive.Inc()
	}
	return err
}

func (o *ObservedBus) QueueSubscribe(topic string, qos byte, group string, handler MessageHandler) error {
	wrapped := func(t string, payload []byte) {
		mqttReceivedTotal.WithLabelValues(normalizeTopic(t)).Inc()
		handler(t, payload)
	}
	err := o.inner.QueueSubscribe(topic, qos, group, wrapped)
	if err != nil {
		observability.RecordError(observability.ErrMQTT, "bus")
	} else {
		mqttSubscriptionsActive.Inc()
	}
	return err
}

func (o *ObservedBus) IsConnected() bool {
	connected := o.inner.IsConnected()
	if connected {
		mqttConnected.Set(1)
		if !o.wasConnected {
			mqttReconnectsTotal.Inc()
			o.wasConnected = true
		}
	} else {
		mqttConnected.Set(0)
		if o.wasConnected {
			o.wasConnected = false
		}
	}
	return connected
}

func (o *ObservedBus) Disconnect() {
	o.inner.Disconnect()
	mqttConnected.Set(0)
	o.wasConnected = false
}

// normalizeTopic replaces device IDs and tenant IDs in topic paths with "+"
// to avoid Prometheus cardinality explosion.
// "meshsat/ABC123/mo/raw" → "meshsat/+/mo/raw"
// "meshsat/tenant1/ABC123/position" → "meshsat/+/+/position"
func normalizeTopic(topic string) string {
	parts := strings.Split(topic, "/")
	if len(parts) < 2 || parts[0] != "meshsat" {
		return topic
	}
	// Replace all variable segments (IDs) with "+".
	for i := 1; i < len(parts); i++ {
		seg := parts[i]
		// Keep known static segments.
		switch seg {
		case "mo", "mt", "raw", "decoded", "send", "status",
			"signal", "health", "position", "telemetry",
			"sos", "config", "current", "update",
			"hub", "events", "credits", "birth", "death",
			"command", "response", "reticulum":
			continue
		default:
			parts[i] = "+"
		}
	}
	return strings.Join(parts, "/")
}
