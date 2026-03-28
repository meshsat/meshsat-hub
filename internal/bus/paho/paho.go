// Package paho implements bus.MessageBus using Eclipse Paho MQTT client.
// Connects to any MQTT broker (Mosquitto, NATS MQTT adapter, etc.).
// Used in all modes — the broker differs, the client doesn't.
package paho

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/cubeos-app/meshsat-hub/internal/bus"
)

// subscription records a Subscribe or QueueSubscribe call for replay on reconnect.
type subscription struct {
	topic   string
	qos     byte
	handler bus.MessageHandler
}

// Bus implements bus.MessageBus using Paho MQTT.
type Bus struct {
	brokerURL string
	clientID  string
	inner     pahomqtt.Client
	connected atomic.Bool

	mu   sync.Mutex
	subs []subscription
}

// TLSConfig holds optional TLS settings for MQTT connections.
type TLSConfig struct {
	CertFile string // Client certificate PEM file
	KeyFile  string // Client private key PEM file
	CAFile   string // CA certificate PEM file for broker verification
}

// New creates a new Paho MQTT bus.
func New(brokerURL, clientID string) *Bus {
	return NewWithTLS(brokerURL, clientID, nil)
}

// NewWithTLS creates a new Paho MQTT bus with optional mutual TLS.
func NewWithTLS(brokerURL, clientID string, tlsCfg *TLSConfig) *Bus {
	b := &Bus{brokerURL: brokerURL, clientID: clientID}

	// Parse credentials from broker URL if present (tcp://user:pass@host:port).
	// Paho's AddBroker only uses the host:port — it ignores userinfo.
	cleanBrokerURL := brokerURL
	if u, err := url.Parse(brokerURL); err == nil && u.User != nil {
		if user := u.User.Username(); user != "" {
			slog.Info("bus: mqtt auth from broker URL", "user", user)
			// Reconstruct URL without userinfo for AddBroker.
			u.User = nil
			cleanBrokerURL = u.String()
		}
	}

	opts := pahomqtt.NewClientOptions().
		AddBroker(cleanBrokerURL).
		SetClientID(clientID).
		SetProtocolVersion(4). // MQTT 3.1.1 — required by NATS MQTT adapter
		SetAutoReconnect(true).
		SetMaxReconnectInterval(30 * time.Second).
		SetKeepAlive(60 * time.Second).
		SetCleanSession(true).
		SetOnConnectHandler(func(c pahomqtt.Client) {
			b.connected.Store(true)
			slog.Info("bus: mqtt connected", "broker", brokerURL)
			b.resubscribe(c)
		}).
		SetConnectionLostHandler(func(_ pahomqtt.Client, err error) {
			b.connected.Store(false)
			slog.Warn("bus: mqtt connection lost", "error", err)
		}).
		SetReconnectingHandler(func(_ pahomqtt.Client, _ *pahomqtt.ClientOptions) {
			slog.Info("bus: mqtt reconnecting", "broker", brokerURL)
		})

	// Apply credentials from URL userinfo (tcp://user:pass@host:port).
	if u, err := url.Parse(brokerURL); err == nil && u.User != nil {
		opts.SetUsername(u.User.Username())
		if pass, ok := u.User.Password(); ok {
			opts.SetPassword(pass)
		}
	}

	if tlsCfg != nil {
		tc, err := loadTLSConfig(tlsCfg)
		if err != nil {
			slog.Error("bus: failed to load TLS config", "error", err)
		} else {
			opts.SetTLSConfig(tc)
			slog.Info("bus: MQTT TLS enabled", "cert", tlsCfg.CertFile, "ca", tlsCfg.CAFile)
		}
	}

	b.inner = pahomqtt.NewClient(opts)
	return b
}

func loadTLSConfig(cfg *TLSConfig) (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}

	if cfg.CAFile != "" {
		caCert, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("invalid CA certificate")
		}
		tlsConfig.RootCAs = pool
	}

	if cfg.CertFile != "" && cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client cert: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	return tlsConfig, nil
}

// resubscribe replays all registered subscriptions after a reconnect.
func (b *Bus) resubscribe(c pahomqtt.Client) {
	b.mu.Lock()
	snapshot := make([]subscription, len(b.subs))
	copy(snapshot, b.subs)
	b.mu.Unlock()

	if len(snapshot) == 0 {
		return
	}

	slog.Info("bus: replaying subscriptions after reconnect", "count", len(snapshot))
	for _, s := range snapshot {
		handler := s.handler // capture for closure
		token := c.Subscribe(s.topic, s.qos, func(_ pahomqtt.Client, msg pahomqtt.Message) {
			handler(msg.Topic(), msg.Payload())
		})
		token.Wait()
		if err := token.Error(); err != nil {
			slog.Error("bus: resubscribe failed", "topic", s.topic, "error", err)
		} else {
			slog.Debug("bus: resubscribed", "topic", s.topic)
		}
	}
}

func (b *Bus) Connect() error {
	// Retry initial connection up to 5 times with backoff.
	// Paho's AutoReconnect only works after a successful initial connection,
	// so we must handle the first connect ourselves.
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			delay := time.Duration(attempt) * 2 * time.Second
			slog.Info("bus: mqtt retrying initial connect", "attempt", attempt+1, "delay", delay)
			time.Sleep(delay)
		}
		token := b.inner.Connect()
		token.Wait()
		if err := token.Error(); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("bus: mqtt connect to %s: %w", b.brokerURL, lastErr)
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
	b.mu.Lock()
	b.subs = append(b.subs, subscription{topic: topic, qos: qos, handler: handler})
	b.mu.Unlock()
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
