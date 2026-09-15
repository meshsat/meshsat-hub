package cloudloop

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/metrics"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// A tenant's own Cloudloop MQTT feed (MESHSAT-1151).
//
// Cloudloop pushes LingoMO messages over mutual-TLS MQTT. Until this the Hub
// held ONE such feed, from certificate files in the environment, and it
// belonged to the platform: a customer could give the Hub its API key for MT
// sends but not its MQTT certificate, so its satellite messages arrived by
// webhook or not at all. The pool copies the APRS-IS shape (aprsis.ConnPool):
// runs on the leader, reconciles the set of tenants that filled in the MQTT
// fields of their Cloudloop account every tick, redials when the account's
// fingerprint changes, and keeps one tenant's failure from touching another.
//
// The platform feed is NOT in this pool. It stays the leader singleton built
// from the environment (cmd/meshsat-hub/main.go), so a non-default tenant
// can never be served the operator's certificate by construction.

type mqttAccounts interface {
	TenantsWith(ctx context.Context, provider string) ([]string, error)
	ForTenant(ctx context.Context, tenantID, provider string) (*integrations.Account, error)
}

type MQTTPool struct {
	accounts mqttAccounts
	handler  func(ctx context.Context, mo *LingoMO) string
	dialWait time.Duration

	mu    sync.Mutex
	feeds map[string]*tenantFeed
}

type tenantFeed struct {
	fingerprint string
	clientID    string
	account     string
	broker      string
	client      paho.Client
}

// FeedStatus is what the pool knows about one tenant's feed, for tests and
// for the operator.
type FeedStatus struct {
	ClientID  string
	Account   string
	Broker    string
	Connected bool
}

func NewMQTTPool(accounts mqttAccounts, handler func(ctx context.Context, mo *LingoMO) string) *MQTTPool {
	return &MQTTPool{accounts: accounts, handler: handler, dialWait: 20 * time.Second, feeds: map[string]*tenantFeed{}}
}

// Run reconciles every tick until ctx ends, then closes every feed. It is
// meant to be a leader singleton: the broker keeps one session per client id.
func (p *MQTTPool) Run(ctx context.Context, tick time.Duration) {
	p.Reconcile(ctx)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			p.Close()
			return
		case <-t.C:
			p.Reconcile(ctx)
		}
	}
}

// feedFields are what a tenant must fill in before the pool dials for it.
var feedFields = []string{"account_id", "mqtt_broker_url", "mqtt_ca_pem", "mqtt_client_cert_pem", "mqtt_client_key_pem"}

// wantsFeed reports whether an account is a complete, tenant-owned MQTT feed.
func wantsFeed(acct *integrations.Account) bool {
	if acct == nil || acct.Platform {
		return false
	}
	for _, k := range feedFields {
		if acct.Get(k) == "" {
			return false
		}
	}
	return true
}

func (p *MQTTPool) Reconcile(ctx context.Context) {
	want := map[string]*integrations.Account{}
	if p.accounts != nil {
		tenants, err := p.accounts.TenantsWith(ctx, integrations.ProviderCloudloop)
		if err != nil {
			slog.Warn("cloudloop mqtt: listing tenants with a Cloudloop account failed", "error", err)
		}
		for _, tid := range tenants {
			if tid == store.DefaultTenantID {
				continue // the platform feed is the environment's, not a row
			}
			acct, err := p.accounts.ForTenant(ctx, tid, integrations.ProviderCloudloop)
			if err != nil || !wantsFeed(acct) {
				continue
			}
			want[tid] = acct
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	for tid, f := range p.feeds {
		if _, keep := want[tid]; !keep {
			slog.Info("cloudloop mqtt: closing a tenant's feed, the account no longer has one", "tenant", tid)
			f.client.Disconnect(500)
			delete(p.feeds, tid)
			metrics.CloudloopMQTTConnected.DeleteLabelValues(tid)
		}
	}

	for tid, acct := range want {
		fp := fingerprint(acct.Get("mqtt_broker_url"), acct.Get("account_id"),
			acct.Get("mqtt_ca_pem"), acct.Get("mqtt_client_cert_pem"), acct.Get("mqtt_client_key_pem"))
		if f, ok := p.feeds[tid]; ok {
			if f.fingerprint == fp {
				continue // paho retries and reconnects by itself; nothing to redo
			}
			slog.Info("cloudloop mqtt: a tenant's feed changed, redialing", "tenant", tid)
			f.client.Disconnect(500)
			delete(p.feeds, tid)
		}
		f, err := p.dial(tid, acct, fp)
		if err != nil {
			metrics.CloudloopMQTTConnectFailures.WithLabelValues(tid).Inc()
			slog.Warn("cloudloop mqtt: a tenant's feed cannot be started", "tenant", tid, "error", err)
			continue
		}
		p.feeds[tid] = f
	}
}

// clientID is unique per tenant AND per account fingerprint, so two tenants
// never evict each other at the broker and a rotated certificate gets a fresh
// session rather than fighting the old one for the same id.
func clientID(tenantID, fingerprint string) string {
	t := sha256.Sum256([]byte(tenantID))
	return "meshsat-hub-cl-" + hex.EncodeToString(t[:4]) + "-" + fingerprint[:8]
}

func (p *MQTTPool) dial(tid string, acct *integrations.Account, fp string) (*tenantFeed, error) {
	tlsCfg, err := tlsFromPEM(acct.Get("mqtt_ca_pem"), acct.Get("mqtt_client_cert_pem"), acct.Get("mqtt_client_key_pem"))
	if err != nil {
		return nil, err
	}
	f := &tenantFeed{
		fingerprint: fp,
		clientID:    clientID(tid, fp),
		account:     acct.Get("account_id"),
		broker:      acct.Get("mqtt_broker_url"),
	}
	topic := fmt.Sprintf("lingo/%s/+/MO", f.account)
	onMessage := func(_ paho.Client, msg paho.Message) {
		var mo LingoMO
		if err := json.Unmarshal(msg.Payload(), &mo); err != nil {
			slog.Warn("cloudloop mqtt: invalid LingoMO JSON on a tenant's feed", "tenant", tid, "error", err, "topic", msg.Topic())
			return
		}
		metrics.CloudloopMQTTMessages.WithLabelValues(tid).Inc()
		slog.Info("cloudloop mqtt: MO received on a tenant's feed", "tenant", tid, "id", mo.ID, "imei", mo.ExtractIMEI())
		// The feed's tenant rides on the context: processLingoMO refuses a
		// device this tenant does not own, so a misconfigured feed cannot file
		// another tenant's messages.
		p.handler(tenancy.WithTenant(context.Background(), tid), &mo)
	}
	opts := paho.NewClientOptions().
		AddBroker(f.broker).
		SetClientID(f.clientID).
		SetTLSConfig(tlsCfg).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(30 * time.Second).
		SetConnectTimeout(15 * time.Second).
		SetConnectionLostHandler(func(_ paho.Client, err error) {
			metrics.CloudloopMQTTConnected.WithLabelValues(tid).Set(0)
			slog.Warn("cloudloop mqtt: a tenant's feed lost its connection", "tenant", tid, "error", err)
		}).
		SetOnConnectHandler(func(c paho.Client) {
			metrics.CloudloopMQTTConnected.WithLabelValues(tid).Set(1)
			tok := c.Subscribe(topic, 1, onMessage)
			tok.Wait()
			if err := tok.Error(); err != nil {
				slog.Error("cloudloop mqtt: subscribe failed on a tenant's feed", "tenant", tid, "topic", topic, "error", err)
				return
			}
			slog.Info("cloudloop mqtt: a tenant's feed is subscribed", "tenant", tid, "topic", topic, "client_id", f.clientID)
		})
	f.client = paho.NewClient(opts)
	metrics.CloudloopMQTTConnected.WithLabelValues(tid).Set(0)
	token := f.client.Connect()
	// ConnectRetry keeps trying in the background; this only reports the first
	// attempt so a wrong broker or refused certificate is visible in the
	// failure counter within a tick rather than never.
	go func() {
		if !token.WaitTimeout(p.dialWait) || token.Error() != nil {
			metrics.CloudloopMQTTConnectFailures.WithLabelValues(tid).Inc()
			slog.Warn("cloudloop mqtt: first connect of a tenant's feed did not succeed, retrying in the background",
				"tenant", tid, "broker", f.broker, "error", token.Error())
		}
	}()
	return f, nil
}

// tlsFromPEM builds the mutual-TLS config from PEM text rather than files:
// the tenant pasted these into the UI and they live encrypted in the database.
func tlsFromPEM(caPEM, certPEM, keyPEM string) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, errors.New("broker CA holds no certificate")
	}
	cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("client certificate and key: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, Certificates: []tls.Certificate{cert}}, nil
}

// Feeds returns a snapshot of the pool.
func (p *MQTTPool) Feeds() map[string]FeedStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]FeedStatus{}
	for tid, f := range p.feeds {
		out[tid] = FeedStatus{ClientID: f.clientID, Account: f.account, Broker: f.broker, Connected: f.client.IsConnected()}
	}
	return out
}

func (p *MQTTPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for tid, f := range p.feeds {
		f.client.Disconnect(500)
		metrics.CloudloopMQTTConnected.DeleteLabelValues(tid)
		delete(p.feeds, tid)
	}
}
