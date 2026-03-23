package cloudloop

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// MQTTSubscriberConfig configures the Cloudloop MQTT subscriber.
type MQTTSubscriberConfig struct {
	BrokerURL  string // e.g., ssl://mqtt.cloudloop.com:8883
	CACertFile string // CA certificate PEM file path
	CertFile   string // Client certificate PEM file path
	KeyFile    string // Client private key PEM file path
	AccountID  string // Cloudloop account ID
	ClientID   string // MQTT client ID (default: meshsat-hub-cloudloop)
}

// MQTTSubscriber connects to the Cloudloop MQTT broker and receives LingoMO messages.
type MQTTSubscriber struct {
	cfg     MQTTSubscriberConfig
	client  paho.Client
	handler func(ctx context.Context, mo *LingoMO) string
	mu      sync.Mutex
	running bool
	stopCh  chan struct{}
}

// NewMQTTSubscriber creates a new Cloudloop MQTT subscriber.
// The handler is called for each received LingoMO message.
func NewMQTTSubscriber(cfg MQTTSubscriberConfig, handler func(ctx context.Context, mo *LingoMO) string) *MQTTSubscriber {
	if cfg.ClientID == "" {
		cfg.ClientID = "meshsat-hub-cloudloop"
	}
	return &MQTTSubscriber{
		cfg:     cfg,
		handler: handler,
		stopCh:  make(chan struct{}),
	}
}

// Start connects to the Cloudloop MQTT broker and subscribes to MO topics.
func (s *MQTTSubscriber) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return fmt.Errorf("cloudloop mqtt: already running")
	}

	tlsConfig, err := s.buildTLSConfig()
	if err != nil {
		return fmt.Errorf("cloudloop mqtt: TLS config: %w", err)
	}

	opts := paho.NewClientOptions().
		AddBroker(s.cfg.BrokerURL).
		SetClientID(s.cfg.ClientID).
		SetTLSConfig(tlsConfig).
		SetAutoReconnect(true).
		SetCleanSession(true).
		SetConnectionLostHandler(func(_ paho.Client, err error) {
			slog.Warn("cloudloop mqtt: connection lost", "error", err)
		}).
		SetOnConnectHandler(func(c paho.Client) {
			slog.Info("cloudloop mqtt: connected, subscribing")
			s.subscribe(c)
		})

	s.client = paho.NewClient(opts)
	token := s.client.Connect()
	token.Wait()
	if err := token.Error(); err != nil {
		return fmt.Errorf("cloudloop mqtt: connect: %w", err)
	}

	s.running = true
	slog.Info("cloudloop mqtt: subscriber started",
		"broker", s.cfg.BrokerURL,
		"account", s.cfg.AccountID,
	)

	return nil
}

// Stop disconnects from the Cloudloop MQTT broker.
func (s *MQTTSubscriber) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return
	}

	close(s.stopCh)
	if s.client != nil && s.client.IsConnected() {
		s.client.Disconnect(1000) // 1s graceful disconnect
	}
	s.running = false
	slog.Info("cloudloop mqtt: subscriber stopped")
}

func (s *MQTTSubscriber) subscribe(c paho.Client) {
	// Subscribe to: lingo/{accountID}/+/MO
	topic := fmt.Sprintf("lingo/%s/+/MO", s.cfg.AccountID)
	token := c.Subscribe(topic, 1, s.onMessage)
	token.Wait()
	if err := token.Error(); err != nil {
		slog.Error("cloudloop mqtt: subscribe failed", "topic", topic, "error", err)
		return
	}
	slog.Info("cloudloop mqtt: subscribed", "topic", topic)
}

func (s *MQTTSubscriber) onMessage(_ paho.Client, msg paho.Message) {
	var mo LingoMO
	if err := json.Unmarshal(msg.Payload(), &mo); err != nil {
		slog.Warn("cloudloop mqtt: invalid LingoMO JSON",
			"error", err, "topic", msg.Topic(), "bytes", len(msg.Payload()))
		return
	}

	slog.Info("cloudloop mqtt: MO received",
		"id", mo.ID,
		"imei", mo.ExtractIMEI(),
		"topic", msg.Topic(),
	)

	// Process through the shared pipeline.
	s.handler(context.Background(), &mo)
}

func (s *MQTTSubscriber) buildTLSConfig() (*tls.Config, error) {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	// Load CA certificate if provided.
	if s.cfg.CACertFile != "" {
		caCert, err := os.ReadFile(s.cfg.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("read CA cert %s: %w", s.cfg.CACertFile, err)
		}
		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA cert %s", s.cfg.CACertFile)
		}
		tlsCfg.RootCAs = caCertPool
	}

	// Load client certificate and key for mutual TLS.
	if s.cfg.CertFile != "" && s.cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(s.cfg.CertFile, s.cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client cert/key: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	return tlsCfg, nil
}
