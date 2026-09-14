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

	"github.com/meshsat/meshsat-hub/internal/bus"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
)

// route holds every handler registered for one exact topic filter.
//
// Paho's client-side router keeps a single callback per exact filter string —
// a second Subscribe on the same filter REPLACES the first callback
// (router.addRoute), silently starving the earlier subscriber. The bus
// therefore registers exactly one Paho subscription per filter, whose
// dispatcher fans out to every handler recorded here (MESHSAT-710).
type route struct {
	qos      byte
	handlers []bus.MessageHandler
}

// Bus implements bus.MessageBus using Paho MQTT.
type Bus struct {
	brokerURL string
	clientID  string
	inner     pahomqtt.Client
	connected atomic.Bool

	// subMu serialises Subscribe and resubscribe so the handler registry and
	// the broker's subscription state cannot diverge under concurrent calls.
	// Never held while dispatching messages.
	subMu sync.Mutex

	mu     sync.Mutex // guards routes
	routes map[string]*route
}

// TLSConfig holds optional TLS settings for MQTT connections.
type TLSConfig struct {
	CertFile string // Client certificate PEM file
	KeyFile  string // Client private key PEM file
	CAFile   string // CA certificate PEM file for broker verification
}

// New creates a new Paho MQTT bus.
// connectBudget is how long Connect() waits before returning to the caller.
// It is a budget, not a deadline: paho keeps retrying afterwards.
const connectBudget = 20 * time.Second

// connectRetryInterval is how often paho retries the initial connection.
const connectRetryInterval = 10 * time.Second

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

	shownURL := redactURL(brokerURL)
	opts := pahomqtt.NewClientOptions().
		AddBroker(cleanBrokerURL).
		SetClientID(clientID).
		SetProtocolVersion(4). // MQTT 3.1.1 — required by NATS MQTT adapter
		SetAutoReconnect(true).
		SetMaxReconnectInterval(30 * time.Second).
		// ConnectRetry makes paho keep trying the FIRST connection too.
		// AutoReconnect alone only covers a connection that once succeeded, so
		// without this a broker that is down at startup is permanent for the
		// life of the process -- and main.go's "will retry in background" was
		// simply untrue (MESHSAT-1129).
		SetConnectRetry(true).
		SetConnectRetryInterval(connectRetryInterval).
		SetKeepAlive(60 * time.Second).
		SetCleanSession(true).
		SetOnConnectHandler(func(c pahomqtt.Client) {
			b.connected.Store(true)
			slog.Info("bus: mqtt connected", "broker", shownURL)
			b.resubscribe(c)
		}).
		SetConnectionLostHandler(func(_ pahomqtt.Client, err error) {
			b.connected.Store(false)
			slog.Warn("bus: mqtt connection lost", "error", err)
		}).
		SetReconnectingHandler(func(_ pahomqtt.Client, _ *pahomqtt.ClientOptions) {
			slog.Info("bus: mqtt reconnecting", "broker", shownURL)
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
	b.subMu.Lock()
	defer b.subMu.Unlock()

	b.mu.Lock()
	filters := make(map[string]byte, len(b.routes))
	for topic, r := range b.routes {
		filters[topic] = r.qos
	}
	b.mu.Unlock()

	if len(filters) == 0 {
		return
	}

	slog.Info("bus: replaying subscriptions after reconnect", "count", len(filters))
	for topic, qos := range filters {
		token := c.Subscribe(topic, qos, b.dispatcherFor(topic))
		token.Wait()
		if err := token.Error(); err != nil {
			slog.Error("bus: resubscribe failed", "topic", topic, "error", err)
		} else {
			slog.Debug("bus: resubscribed", "topic", topic)
		}
	}
}

// dispatcherFor returns the single Paho callback shared by all handlers of one
// topic filter: it snapshots the filter's handlers under the lock and invokes
// each in registration order, outside the lock.
func (b *Bus) dispatcherFor(filter string) pahomqtt.MessageHandler {
	return func(_ pahomqtt.Client, msg pahomqtt.Message) {
		b.mu.Lock()
		var handlers []bus.MessageHandler
		if r, ok := b.routes[filter]; ok {
			handlers = make([]bus.MessageHandler, len(r.handlers))
			copy(handlers, r.handlers)
		}
		b.mu.Unlock()
		for _, h := range handlers {
			h(msg.Topic(), msg.Payload())
		}
	}
}

func (b *Bus) Connect() error {
	// Paho retries the initial connect itself now (SetConnectRetry above), so
	// this waits for a budget and then hands back to the background rather than
	// giving up. The old version tried five times over about twenty seconds and
	// then stopped forever, which is what made a broker blip at startup a
	// permanent condition: no reconnect, therefore no OnConnect, therefore no
	// resubscribe, therefore no subscriber on that replica at all.
	token := b.inner.Connect()
	if !token.WaitTimeout(connectBudget) {
		return fmt.Errorf("bus: mqtt not connected to %s within %s; paho is still retrying "+
			"every %s and subscriptions will be replayed when it succeeds",
			redactURL(b.brokerURL), connectBudget, connectRetryInterval)
	}
	if err := token.Error(); err != nil {
		return fmt.Errorf("bus: mqtt connect to %s: %w", redactURL(b.brokerURL), err)
	}
	return nil
}

func (b *Bus) Publish(topic string, qos byte, retained bool, payload []byte) error {
	// A wildcard in a publish topic is refused by the broker, which drops the
	// connection; paho then resends the same publish on every reconnect and
	// the replica never recovers (MESHSAT-1022). Refuse it here instead.
	if err := hubmqtt.CheckPublishTopic(topic); err != nil {
		return fmt.Errorf("bus: publish: %w", err)
	}
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
	b.subMu.Lock()
	defer b.subMu.Unlock()

	b.mu.Lock()
	if b.routes == nil {
		b.routes = make(map[string]*route)
	}
	r, exists := b.routes[topic]
	subQoS := qos
	if exists && r.qos > subQoS {
		subQoS = r.qos
	}
	// The broker needs a (re)subscribe only for a new filter or a QoS upgrade;
	// otherwise the existing shared subscription already covers this handler.
	needSubscribe := !exists || subQoS > r.qos
	b.mu.Unlock()

	// NOT CONNECTED IS NOT AN ERROR. Connect() failure at startup is a warning
	// retried in the background, so an unconnected bus here is an ordinary state.
	//
	// Register the handler and return successfully: resubscribe() replays every
	// filter in b.routes when the connection comes up, so a registered handler
	// reaches the broker by itself. Returning an error instead is what made
	// MESHSAT-1129, and it failed twice over -- the handler was never registered,
	// AND every caller does `return err` on the first filter, so the second half
	// of a DualFilters pair was abandoned too.
	//
	// A broker that is CONNECTED and still refuses is a real error and is
	// reported as one, so a bad filter still fails loudly.
	if needSubscribe {
		if !b.connected.Load() {
			slog.Warn("bus: not connected yet; the handler is registered and will be "+
				"sent to the broker when the connection comes up", "topic", topic)
		} else {
			token := b.inner.Subscribe(topic, subQoS, b.dispatcherFor(topic))
			token.Wait()
			if err := token.Error(); err != nil {
				return fmt.Errorf("bus: subscribe %s: %w", topic, err)
			}
		}
	}

	b.mu.Lock()
	if r == nil {
		r = &route{}
		b.routes[topic] = r
	}
	r.qos = subQoS
	r.handlers = append(r.handlers, handler)
	count := len(r.handlers)
	b.mu.Unlock()

	slog.Debug("bus: subscribed", "topic", topic, "qos", subQoS, "handlers", count)
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

// redactURL returns the broker URL with any password replaced by "xxxxx"
// (net/url's Redacted form). Broker URLs carry the NATS MQTT credentials in
// their userinfo, and they used to be printed verbatim into the connect
// error and the connected/reconnecting log lines.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid broker url>"
	}
	return u.Redacted()
}
