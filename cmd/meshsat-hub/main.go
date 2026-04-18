package main

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cubeos-app/meshsat-hub/cmd/meshsat-hub/web"
	"github.com/cubeos-app/meshsat-hub/internal/alerting"
	"github.com/cubeos-app/meshsat-hub/internal/api"
	"github.com/cubeos-app/meshsat-hub/internal/apprise"
	"github.com/cubeos-app/meshsat-hub/internal/aprsis"
	"github.com/cubeos-app/meshsat-hub/internal/audit"
	hubauth "github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/backup"
	"github.com/cubeos-app/meshsat-hub/internal/bridge"
	"github.com/cubeos-app/meshsat-hub/internal/bus"
	"github.com/cubeos-app/meshsat-hub/internal/bus/paho"
	"github.com/cubeos-app/meshsat-hub/internal/cloudloop"
	"github.com/cubeos-app/meshsat-hub/internal/cluster"
	"github.com/cubeos-app/meshsat-hub/internal/codec"
	"github.com/cubeos-app/meshsat-hub/internal/config"
	"github.com/cubeos-app/meshsat-hub/internal/constellation"
	hubcrypto "github.com/cubeos-app/meshsat-hub/internal/crypto"
	"github.com/cubeos-app/meshsat-hub/internal/deadman"
	"github.com/cubeos-app/meshsat-hub/internal/dedup"
	"github.com/cubeos-app/meshsat-hub/internal/directory"
	hubemail "github.com/cubeos-app/meshsat-hub/internal/email"
	"github.com/cubeos-app/meshsat-hub/internal/escalation"
	"github.com/cubeos-app/meshsat-hub/internal/fragment"
	"github.com/cubeos-app/meshsat-hub/internal/geo"
	"github.com/cubeos-app/meshsat-hub/internal/globalstar"
	"github.com/cubeos-app/meshsat-hub/internal/hawkbit"
	"github.com/cubeos-app/meshsat-hub/internal/health"
	"github.com/cubeos-app/meshsat-hub/internal/ipougrs"
	"github.com/cubeos-app/meshsat-hub/internal/leader"
	hubmessage "github.com/cubeos-app/meshsat-hub/internal/message"
	"github.com/cubeos-app/meshsat-hub/internal/metrics"
	hubmw "github.com/cubeos-app/meshsat-hub/internal/middleware"
	"github.com/cubeos-app/meshsat-hub/internal/mptcp"
	hubmsvqsc "github.com/cubeos-app/meshsat-hub/internal/msvqsc"
	"github.com/cubeos-app/meshsat-hub/internal/ntfy"
	"github.com/cubeos-app/meshsat-hub/internal/observability"
	"github.com/cubeos-app/meshsat-hub/internal/position"
	"github.com/cubeos-app/meshsat-hub/internal/protocol"
	"github.com/cubeos-app/meshsat-hub/internal/ratelimit"
	"github.com/cubeos-app/meshsat-hub/internal/reticulum"
	"github.com/cubeos-app/meshsat-hub/internal/rock7"
	"github.com/cubeos-app/meshsat-hub/internal/rockblock"
	"github.com/cubeos-app/meshsat-hub/internal/routing"
	"github.com/cubeos-app/meshsat-hub/internal/scheduler"
	"github.com/cubeos-app/meshsat-hub/internal/sms"
	"github.com/cubeos-app/meshsat-hub/internal/sos"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/cubeos-app/meshsat-hub/internal/store/mariadb"
	"github.com/cubeos-app/meshsat-hub/internal/store/sqlite"
	"github.com/cubeos-app/meshsat-hub/internal/tak"
	"github.com/cubeos-app/meshsat-hub/internal/timesync"
	hubtor "github.com/cubeos-app/meshsat-hub/internal/tor"
	"github.com/cubeos-app/meshsat-hub/internal/webhook"
	"github.com/cubeos-app/meshsat-hub/internal/wireguard"

	swaggerDocs "github.com/cubeos-app/meshsat-hub/docs/swagger"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/redis/go-redis/v9"
)

var version = "dev"

// costRecorderAdapter adapts a store.Store to cloudloop.CostRecorder.
type costRecorderAdapter struct {
	store store.Store
}

func (a *costRecorderAdapter) InsertCostEntry(ctx context.Context, tenantID string, c *cloudloop.CostEntry) error {
	sc := &store.CostEntry{
		ID:            c.ID,
		DeviceIMEI:    c.DeviceIMEI,
		InterfaceType: c.InterfaceType,
		Direction:     c.Direction,
		CostUSD:       c.CostUSD,
		MessageID:     c.MessageID,
		Detail:        c.Detail,
	}
	if tenantID == "" {
		tenantID = store.DefaultTenantID
	}
	return a.store.InsertCostEntry(ctx, tenantID, sc)
}

// escalationAdapter adapts escalation.Engine to alerting.EscalationTrigger.
type escalationAdapter struct {
	engine *escalation.Engine
}

func (a *escalationAdapter) Trigger(ctx context.Context, tenantID, chainID, deviceIMEI, alertType, detail string) error {
	alert := &store.Alert{
		ChainID:    chainID,
		DeviceIMEI: deviceIMEI,
		Type:       alertType,
		Detail:     detail,
	}
	return a.engine.Trigger(ctx, tenantID, alert)
}

// federationBusAdapter wraps bus.MessageBus to satisfy tak.FederationBus.
// Needed because bus.MessageBus.Subscribe takes a named MessageHandler type
// while tak.FederationBus.Subscribe takes func(string, []byte) directly.
type federationBusAdapter struct {
	mb bus.MessageBus
}

func (a *federationBusAdapter) Publish(topic string, qos byte, retained bool, payload []byte) error {
	return a.mb.Publish(topic, qos, retained, payload)
}

func (a *federationBusAdapter) Subscribe(topic string, qos byte, handler func(string, []byte)) error {
	return a.mb.Subscribe(topic, qos, handler)
}

// @title        MeshSat Hub API
// @version      1.1
// @description  Multi-tenant SaaS platform for satellite device management. Ingests MO messages from Iridium/Globalstar, manages devices, SOS escalation, dead man's switch, and E2E encryption.
// @license.name Apache 2.0
// @license.url  https://www.apache.org/licenses/LICENSE-2.0
// @host         localhost:6070
// @BasePath     /
// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	initLogger(cfg)
	slog.Info("starting meshsat-hub", "version", version, "port", cfg.Port, "mode", cfg.Mode)

	// Observability: build info metric and OTel tracing.
	metrics.SetBuildInfo(version, cfg.Mode, fmt.Sprintf("go%d.%d", 1, 25))
	otelShutdown, _ := observability.InitTracing(context.Background(), cfg.OTelServiceName, cfg.OTelEndpoint)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	probeTimeout, _ := time.ParseDuration(cfg.HealthProbeTimeout)
	checker := health.New(probeTimeout)

	// --- Message bus (tri-mode) ---
	var msgBus bus.MessageBus
	switch cfg.Mode {
	case "cluster", "kubernetes":
		// In cluster/k8s mode, Paho connects to the external NATS MQTT port.
		brokerURL := cfg.MQTTBrokerURL
		if cfg.NATSUrl != "" {
			brokerURL = cfg.NATSUrl
		}
		msgBus = paho.New(brokerURL, cfg.MQTTClientID)
	default: // "standalone"
		msgBus = paho.New(cfg.MQTTBrokerURL, cfg.MQTTClientID)
	}
	msgBus = bus.NewObservedBus(msgBus) // Wrap with metrics instrumentation.
	if err := msgBus.Connect(); err != nil {
		slog.Warn("bus connection failed (will retry in background)", "error", err)
	}
	checker.AddProbe("mqtt", func(_ context.Context) error {
		if !msgBus.IsConnected() {
			return fmt.Errorf("mqtt not connected")
		}
		return nil
	})

	// --- Store (tri-mode) ---
	var dataStore store.Store
	var clusterMonitor *cluster.Monitor
	switch cfg.Mode {
	case "cluster", "kubernetes":
		slowQ := time.Duration(cfg.DBSlowQueryMS) * time.Millisecond
		dbStore, err := mariadb.New(cfg.DatabaseURL, slowQ)
		if err != nil {
			slog.Error("mariadb connection failed", "error", err)
			os.Exit(1)
		}
		if err := dbStore.Migrate(ctx); err != nil {
			slog.Error("mariadb migration failed", "error", err)
			os.Exit(1)
		}
		dataStore = dbStore
		checker.AddProbe("mariadb", dbStore.GaleraReady)
		// Cluster health monitor
		var peers []string
		if cfg.ClusterPeers != "" {
			for _, p := range strings.Split(cfg.ClusterPeers, ",") {
				p = strings.TrimSpace(p)
				if p != "" {
					peers = append(peers, p)
				}
			}
		}
		clusterMonitor = cluster.NewMonitor(dbStore.RawDB(), cfg.MQTTClientID, "", peers)
	default: // "standalone"
		slowQ := time.Duration(cfg.DBSlowQueryMS) * time.Millisecond
		sqlStore, err := sqlite.New("/data/hub.db", slowQ)
		if err != nil {
			slog.Error("sqlite open failed", "error", err)
			os.Exit(1)
		}
		if err := sqlStore.Migrate(ctx); err != nil {
			slog.Error("sqlite migration failed", "error", err)
			os.Exit(1)
		}
		dataStore = sqlStore
	}
	defer func() { _ = dataStore.Close() }()

	// Audit service (tamper-evident hash chain).
	auditSvc := audit.New(dataStore)
	// Audit log retention (background goroutine).
	go audit.RunRetention(ctx, dataStore, audit.RetentionConfig{
		RetentionDays: cfg.AuditRetentionDays,
		ArchivePath:   cfg.AuditArchivePath,
	})

	// --- Dedup (tri-mode) ---
	var dedupTracker dedup.Dedup
	switch cfg.Mode {
	case "cluster", "kubernetes":
		redisOpts, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			slog.Error("invalid redis URL", "error", err)
			os.Exit(1)
		}
		redisClient := redis.NewClient(redisOpts)
		dedupTracker = dedup.NewRedisDedup(redisClient, 1*time.Hour, "dedup:")
		checker.AddProbe("redis", func(ctx context.Context) error {
			return redisClient.Ping(ctx).Err()
		})
	default:
		dedupTracker = dedup.NewMemoryDedup(1 * time.Hour)
	}

	// --- Rate limiter (tri-mode) ---
	var limiter ratelimit.Limiter
	switch cfg.Mode {
	case "cluster", "kubernetes":
		redisOpts, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			slog.Error("invalid redis URL for ratelimit", "error", err)
			os.Exit(1)
		}
		redisClient := redis.NewClient(redisOpts)
		limiter = ratelimit.NewRedisLimiter(redisClient, cfg.RateLimitDailyCap, cfg.RateLimitMonthlyCap)
	default:
		limiter = ratelimit.NewDeviceLimiter(
			float64(cfg.RateLimitBurst),  // max burst
			cfg.RateLimitRefillPerMin/60, // tokens per second
			cfg.RateLimitDailyCap,        // daily cap
			cfg.RateLimitMonthlyCap,      // monthly cap
			msgBus,                       // for MQTT alerts
		)
	}
	rateLimitHandler := ratelimit.NewHandler(limiter)

	// --- Leader election (tri-mode) ---
	var leaderElector leader.Leader
	instanceID := fmt.Sprintf("%s-%d", cfg.MQTTClientID, os.Getpid())
	switch cfg.Mode {
	case "cluster":
		leaderElector = leader.NewNATS(msgBus, instanceID)
	case "kubernetes":
		leaderElector = leader.NewKubeLease(instanceID)
	default: // "standalone"
		leaderElector = leader.NewNoop()
	}

	// Cloudloop API client for MT sends.
	cloudloopClient := cloudloop.NewClient(cfg.CloudloopAPIURL, cfg.CloudloopAPIKey)

	// Device resolver: learns IMEI-to-thingID mappings from MO messages and Cloudloop API.
	thingResolver := cloudloop.NewThingResolver(cloudloopClient)
	if cfg.CloudloopAPIKey != "" {
		go thingResolver.StartPeriodicRefresh(ctx, 5*time.Minute)
	}

	// Start MT sender (subscribes to meshsat/+/mt/send).
	mtSender := cloudloop.NewSender(cloudloopClient, msgBus)
	mtSender.SetRateLimiter(limiter)
	mtSender.SetAudit(auditSvc)
	mtSender.SetDeviceResolver(thingResolver)
	mtSender.SetCostRecorder(&costRecorderAdapter{store: dataStore})
	mtSender.SetCostPerMessage(0.05) // Iridium default
	if msgBus.IsConnected() {
		if err := mtSender.Start(); err != nil {
			slog.Error("failed to start MT sender", "error", err)
		}
	}

	// Credit balance poller (polls Cloudloop API, publishes to meshsat/hub/credits).
	if cfg.CloudloopAPIKey != "" && msgBus.IsConnected() {
		creditPoller := cloudloop.NewCreditPoller(cloudloopClient, msgBus, 1*time.Hour)
		go creditPoller.Start(ctx)
	}

	// Globalstar API client (optional — second satellite constellation).
	var globalstarClient *globalstar.Client
	if cfg.GlobalstarAPIKey != "" {
		globalstarClient = globalstar.NewClient(cfg.GlobalstarAPIURL, cfg.GlobalstarAPIKey)
		slog.Info("globalstar: API client enabled", "url", cfg.GlobalstarAPIURL)
	}

	// Constellation router — multi-backend satellite send.
	constellationRouter := constellation.NewRouter(constellation.StrategyAvailable)
	constellationRouter.Register(constellation.NewIridiumBackend(cloudloopClient))
	if globalstarClient != nil {
		constellationRouter.Register(constellation.NewGlobalstarBackend(globalstarClient))
	}

	// MPTCP concentrator monitor (aggregates satellite + cellular links).
	mptcpMonitor := mptcp.NewMonitor(30*time.Second, msgBus)
	go mptcpMonitor.Start(ctx)

	// TAK/CoT gateway starts unconditionally (each site has its own OTS, no conflict).
	// TAK Federation and APRS-IS remain singletons inside leader election.
	var takClient *tak.Client
	if cfg.TAKEnabled && cfg.TAKHost != "" {
		takPort := cfg.TAKPort
		if takPort == 0 {
			takPort = 8087
		}
		takClient = tak.NewClient(cfg.TAKHost, takPort, cfg.TAKSSL)
		if err := takClient.Connect(); err != nil {
			slog.Warn("tak: connection failed (will not forward CoT)", "error", err)
		} else if msgBus.IsConnected() {
			takSub := tak.NewSubscriber(msgBus, takClient, cfg.TAKCallsignPrefix, cfg.TAKCotStaleSec)
			if err := takSub.Start(); err != nil {
				slog.Error("tak: failed to start subscriber", "error", err)
			} else {
				slog.Info("tak: CoT gateway started", "host", cfg.TAKHost, "port", takPort)
			}
		}
	}

	// OTS REST API poller — inbound CoT relay (OTS → Hub → MQTT → bridges).
	// The TCP connection above is Hub→OTS only. OTS plain TCP does not relay
	// events back. This poller provides the reverse path via the REST API.
	var otsPoller *tak.OTSPoller
	if cfg.TAKAPIBaseURL != "" && cfg.TAKAPIUsername != "" {
		otsPoller = tak.NewOTSPoller(cfg.TAKAPIBaseURL, cfg.TAKAPIUsername, cfg.TAKAPIPassword, cfg.TAKAPIPollSec, msgBus, dataStore, store.DefaultTenantID)
		otsPoller.Start()
	}

	var takFederation *tak.Federation
	var aprsisClient *aprsis.Client

	go leaderElector.Run(ctx, func() {
		// onAcquired: start singleton services (Federation + APRS-IS only)
		slog.Info("leader acquired — starting Federation and APRS-IS")

		// TAK Federation v2 (optional — bidirectional CoT relay with remote TAK servers).
		if cfg.TAKFederationEnabled && msgBus.IsConnected() {
			fedCfg := tak.FederationConfig{
				Enabled:        true,
				Port:           cfg.TAKFederationPort,
				Peers:          cfg.TAKFederationPeers,
				CertFile:       cfg.TAKFederationCert,
				KeyFile:        cfg.TAKFederationKey,
				CAFile:         cfg.TAKFederationCA,
				CallsignPrefix: cfg.TAKCallsignPrefix,
				CotStaleSec:    cfg.TAKCotStaleSec,
			}
			takFederation = tak.NewFederation(fedCfg, &federationBusAdapter{mb: msgBus})
			if err := takFederation.Start(ctx); err != nil {
				slog.Error("tak federation: failed to start", "error", err)
				takFederation = nil
			}
		}

		// APRS-IS IGate (optional — inject satellite positions into APRS-IS network).
		if cfg.APRSISEnabled && cfg.APRSISCallsign != "" && cfg.APRSISPasscode != "" {
			server := cfg.APRSISServer
			if server == "" {
				server = "euro.aprs2.net:14580"
			}
			aprsisClient = aprsis.NewClient(server, cfg.APRSISCallsign, 10, cfg.APRSISPasscode, "")
			if err := aprsisClient.Connect(); err != nil {
				slog.Warn("aprsis: connection failed (will not inject positions)", "error", err)
			} else if msgBus.IsConnected() {
				aprsisSub := aprsis.NewSubscriber(msgBus, aprsisClient, 60)
				if err := aprsisSub.Start(); err != nil {
					slog.Error("aprsis: failed to start subscriber", "error", err)
				}
			}
		}
	}, func() {
		// onLost: stop singleton services
		slog.Info("leader lost — stopping TAK, Federation, and APRS-IS")
		if aprsisClient != nil {
			aprsisClient.Disconnect()
			aprsisClient = nil
		}
		if takFederation != nil {
			takFederation.Stop()
			takFederation = nil
		}
		if takClient != nil {
			takClient.Disconnect()
			takClient = nil
		}
	})

	// Outbound webhook dispatcher (fires on MO, SOS, position, telemetry, MT status).
	webhookDispatcher := webhook.NewDispatcher(msgBus)
	webhookAPIHandler := webhook.NewAPIHandler(webhookDispatcher)
	if msgBus.IsConnected() {
		if err := webhookDispatcher.Start(msgBus); err != nil {
			slog.Error("webhook: failed to start dispatcher", "error", err)
		}
	}

	// Position subscriber: stores MQTT position updates to the database.
	var posSub *position.Subscriber
	if msgBus.IsConnected() {
		posSub = position.NewSubscriber(msgBus, dataStore, store.DefaultTenantID)
		if err := posSub.Start(); err != nil {
			slog.Error("position: failed to start subscriber", "error", err)
		}
	}

	// Message subscriber: persists MO decoded messages from MQTT to the database.
	if msgBus.IsConnected() {
		msgSub := hubmessage.NewSubscriber(msgBus, dataStore, store.DefaultTenantID)
		if err := msgSub.Start(); err != nil {
			slog.Error("message: failed to start subscriber", "error", err)
		}
	}

	// Bridge lifecycle subscriber: auto-provisions bridges and devices from MQTT birth/death/health.
	var bridgeCommander *bridge.Commander
	var bridgeSub *bridge.Subscriber
	var hembReassemblyBuf *protocol.HeMBReassemblyBuffer
	if msgBus.IsConnected() {
		bridgeSub = bridge.NewSubscriber(msgBus, dataStore, store.DefaultTenantID)

		// HeMB reassembly: decode bonded RLNC-coded symbols from bridges.
		hembReassemblyBuf = protocol.NewHeMBReassemblyBuffer(nil) // deliverFn set via subscriber handler
		bridgeSub.SetHeMBReassembler(hembReassemblyBuf)

		if err := bridgeSub.Start(); err != nil {
			slog.Error("bridge: failed to start subscriber", "error", err)
		}
		defer bridgeSub.Stop()

		// Bridge commander: sends commands to bridges and correlates responses.
		bridgeCommander = bridge.NewCommander(msgBus, dataStore)
		if err := bridgeCommander.Start(); err != nil {
			slog.Error("bridge: failed to start commander", "error", err)
		}
		defer bridgeCommander.Stop()

		// HeMB reassembly reaper: purge stale streams and update metrics.
		go func() {
			ticker := time.NewTicker(60 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if n := hembReassemblyBuf.Reap(); n > 0 {
						slog.Info("hemb: reaped stale streams", "count", n)
						metrics.HeMBStaleStreamsPurged.Add(float64(n))
					}
					stats := hembReassemblyBuf.Stats()
					metrics.HeMBActiveStreams.Set(float64(stats.ActiveStreams))
					metrics.HeMBReassemblyPending.Set(float64(stats.GenerationsPending))
				}
			}
		}()
	}

	// Bridge reaper: marks bridges offline when last_seen exceeds timeout.
	if cfg.BridgeOfflineTimeout > 0 {
		reaper := bridge.NewReaper(dataStore, time.Duration(cfg.BridgeOfflineTimeout)*time.Second)
		reaper.Start()
		defer reaper.Stop()
	}

	// Bridge certificate authority for MQTT TLS client certs.
	var bridgeCA *bridge.CertAuthority
	caCertPath := os.Getenv("MESHSAT_BRIDGE_CA_CERT")
	caKeyPath := os.Getenv("MESHSAT_BRIDGE_CA_KEY")
	if caCertPath != "" && caKeyPath != "" {
		certPEM, err := os.ReadFile(caCertPath)
		if err != nil {
			slog.Error("bridge-ca: failed to read CA cert", "path", caCertPath, "error", err)
		} else {
			keyPEM, err := os.ReadFile(caKeyPath)
			if err != nil {
				slog.Error("bridge-ca: failed to read CA key", "path", caKeyPath, "error", err)
			} else {
				bridgeCA, err = bridge.NewCertAuthority(certPEM, keyPEM)
				if err != nil {
					slog.Error("bridge-ca: failed to load CA", "error", err)
				} else {
					slog.Info("bridge-ca: loaded certificate authority", "cert", caCertPath)
				}
			}
		}
	} else {
		// Auto-generate self-signed CA and persist via system config.
		caCertVal, _ := dataStore.GetSystemConfig(ctx, "bridge_ca_cert")
		caKeyVal, _ := dataStore.GetSystemConfig(ctx, "bridge_ca_key")
		if caCertVal != "" && caKeyVal != "" {
			bridgeCA, err = bridge.NewCertAuthority([]byte(caCertVal), []byte(caKeyVal))
			if err != nil {
				slog.Error("bridge-ca: failed to load stored CA", "error", err)
			} else {
				slog.Info("bridge-ca: loaded CA from system config")
			}
		} else {
			var certPEM, keyPEM []byte
			bridgeCA, certPEM, keyPEM, err = bridge.NewSelfSignedCA("MeshSat Hub")
			if err != nil {
				slog.Error("bridge-ca: failed to generate self-signed CA", "error", err)
			} else {
				_ = dataStore.SetSystemConfig(ctx, "bridge_ca_cert", string(certPEM))
				_ = dataStore.SetSystemConfig(ctx, "bridge_ca_key", string(keyPEM))
				slog.Info("bridge-ca: generated and stored self-signed CA")
			}
		}
	}

	// Directory-signing trust anchor (MESHSAT-539): bridges pin this pubkey
	// on first provision and use it to verify directory snapshots offline.
	directoryTrustAnchor, err := directory.LoadOrCreateTrustAnchor(ctx, dataStore)
	if err != nil {
		slog.Error("directory-trust-anchor: failed to initialise", "error", err)
	} else {
		slog.Info("directory-trust-anchor: ready", "pubkey_bytes", len(directoryTrustAnchor.PublicKey()))
	}

	// Export bridge CA cert to filesystem for NATS mTLS verification.
	// NATS reads this file at startup to verify bridge client certificates.
	// The export path is typically a shared volume between Hub and NATS containers.
	if bridgeCA != nil && cfg.BridgeCACertExportPath != "" {
		if err := os.WriteFile(cfg.BridgeCACertExportPath, bridgeCA.CACertPEM(), 0644); err != nil {
			slog.Error("bridge-ca: failed to export CA cert for NATS mTLS", "path", cfg.BridgeCACertExportPath, "error", err)
		} else {
			slog.Info("bridge-ca: exported CA cert for NATS mTLS", "path", cfg.BridgeCACertExportPath)
		}
	}

	// Escalation engine (SOS, dead man's switch, custom alerts).
	var notifiers []escalation.Notifier
	if cfg.AppriseEnabled && cfg.AppriseURL != "" {
		appriseClient := apprise.New(cfg.AppriseURL)
		notifiers = append(notifiers, appriseClient)
		checker.AddProbe("apprise", appriseClient.Healthz)
		slog.Info("apprise: notification backend enabled", "url", cfg.AppriseURL)
	}
	if cfg.NtfyEnabled && cfg.NtfyURL != "" {
		ntfyClient := ntfy.New(cfg.NtfyURL)
		if cfg.NtfyToken != "" {
			ntfyClient.SetToken(cfg.NtfyToken)
		}
		notifiers = append(notifiers, ntfyClient)
		checker.AddProbe("ntfy", ntfyClient.Healthz)
		slog.Info("ntfy: notification backend enabled", "url", cfg.NtfyURL)
	}
	if cfg.SMSEnabled && cfg.SMSAccountSID != "" {
		smsNotifier := sms.NewNotifier(sms.NewClient(cfg.SMSAccountSID, cfg.SMSAuthToken, cfg.SMSFromNumber))
		notifiers = append(notifiers, smsNotifier)
		slog.Info("sms: escalation notifier enabled", "from", cfg.SMSFromNumber)
	}
	var emailKeyRing *hubemail.KeyRing
	if cfg.EmailEnabled && cfg.EmailSMTPHost != "" {
		var err error
		emailKeyRing, err = hubemail.NewKeyRing("MeshSat Hub", cfg.EmailFrom, cfg.EmailPGPKey)
		if err != nil {
			slog.Error("email: PGP keyring init failed", "error", err)
		} else {
			emailClient := hubemail.NewClient(cfg.EmailSMTPHost, cfg.EmailFrom, cfg.EmailUsername, cfg.EmailPassword, emailKeyRing)
			notifiers = append(notifiers, hubemail.NewNotifier(emailClient))
			slog.Info("email: escalation notifier enabled", "from", cfg.EmailFrom)
		}
	}
	var escNotifier escalation.Notifier
	switch len(notifiers) {
	case 0:
		// nil = LogNotifier fallback
	case 1:
		escNotifier = notifiers[0]
	default:
		escNotifier = escalation.NewMultiNotifier(notifiers...)
	}
	escEngine := escalation.New(dataStore, escNotifier)
	go escEngine.Start(ctx)

	// Alert rules evaluator (configurable alerting engine, MESHSAT-313).
	alertEval := alerting.New(dataStore, &escalationAdapter{engine: escEngine}, 60*time.Second)
	go alertEval.Start(ctx)

	// Dead man's switch monitor (triggers escalation on missed device check-ins).
	deadmanMonitor := deadman.NewMonitor(dataStore, escEngine)
	go deadmanMonitor.Start(ctx)

	// Wire dead man's switch to position subscriber so device positions reset the timer.
	if posSub != nil {
		posSub.SetDeadman(deadmanMonitor)
	}

	// SOS detector (subscribes to mo/decoded, triggers escalation on SOS messages).
	if msgBus.IsConnected() {
		sosDetector := sos.NewDetector(msgBus, escEngine, dataStore, store.DefaultTenantID, cfg.SOSChainID)
		if err := sosDetector.Start(); err != nil {
			slog.Error("sos: failed to start detector", "error", err)
		} else {
			slog.Info("sos: detector started")
		}
	}

	// Fragment reassembler for multi-fragment MO messages.
	reassembler := fragment.NewReassembler(5 * time.Minute)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n := reassembler.Expire(); n > 0 {
					slog.Info("fragment: expired stale reassemblies", "count", n)
				}
			}
		}
	}()

	// E2E encryption keystore (in-memory, hydrated from database on startup).
	keyStore := hubcrypto.NewKeyStore()
	if devs, err := dataStore.ListDevices(ctx, store.DefaultTenantID); err == nil {
		for _, dev := range devs {
			dk, err := dataStore.GetDeviceKeyLatest(ctx, store.DefaultTenantID, dev.IMEI)
			if err != nil || dk.Mode != "decrypt" || dk.KeyHex == "" {
				continue
			}
			keyBytes, err := hex.DecodeString(dk.KeyHex)
			if err != nil {
				continue
			}
			if _, err := keyStore.StoreKey(dev.IMEI, keyBytes, dk.Mode); err != nil {
				slog.Warn("crypto: failed to load key for device", "imei", dev.IMEI, "error", err)
			}
		}
		slog.Info("crypto: keystore hydrated", "devices_with_keys", keyStore.DeviceCount())
	}
	// Hydrate global keys (e.g. "sms") that aren't tied to a device IMEI.
	for _, globalKeyID := range []string{"sms"} {
		dk, err := dataStore.GetDeviceKeyLatest(ctx, store.DefaultTenantID, globalKeyID)
		if err != nil || dk.Mode != "decrypt" || dk.KeyHex == "" {
			continue
		}
		keyBytes, err := hex.DecodeString(dk.KeyHex)
		if err != nil {
			continue
		}
		if _, err := keyStore.StoreKey(globalKeyID, keyBytes, dk.Mode); err != nil {
			slog.Warn("crypto: failed to load global key", "id", globalKeyID, "error", err)
		} else {
			slog.Info("crypto: global key loaded", "id", globalKeyID)
		}
	}

	// Reticulum identity (Hub's network identity for routing).
	hubIdentity, err := reticulum.NewHubIdentity(dataStore, cfg.ReticulumIdentityFile, cfg.ReticulumAppName)
	if err != nil {
		slog.Error("reticulum: failed to initialize identity", "error", err)
	}
	checker.AddProbe("reticulum_identity", func(_ context.Context) error {
		if hubIdentity == nil || !hubIdentity.IsLoaded() {
			return fmt.Errorf("reticulum identity not loaded")
		}
		return nil
	})

	// Reticulum routing table.
	reticulumRouter := reticulum.NewRouter(reticulum.DefaultRouteTTL)

	// Wire Reticulum router to bridge subscriber so bridge births inject routes.
	if bridgeSub != nil {
		bridgeSub.SetReticulumRouter(reticulumRouter)
	}

	// Wire bridge CA to subscriber for birth signature verification.
	if bridgeSub != nil && bridgeCA != nil {
		bridgeSub.SetCertAuthority(bridgeCA)
		mode := os.Getenv("HUB_BIRTH_SIGNATURE_MODE")
		if mode == "" {
			mode = bridge.BirthSignatureModeWarn
		}
		bridgeSub.SetBirthSignatureMode(mode)
		slog.Info("bridge: birth signature verification enabled", "mode", mode)
	}

	// Reticulum relay — forwards packets between interfaces.
	reticulumRelay := reticulum.NewRelay(reticulumRouter, reticulum.DefaultRelayConfig())

	// Reticulum path handler — responds to path requests from bridges.
	reticulumPathHandler := reticulum.NewPathHandler(
		reticulumRouter, reticulumRelay, reticulum.DefaultPathHandlerConfig(),
	)

	// Time sync service — Hub is the NTP authority (stratum 1).
	hubTimeService := timesync.NewTimeService(nil)
	hubTimeService.AddSource(timesync.NewLocalNTPSource())
	hubTimeService.Start(ctx)
	slog.Info("timesync: hub service started (NTP authority)")

	// DTN custody manager — Hub always accepts custody (relay of last resort) [MESHSAT-491].
	custodyMgr := protocol.NewCustodyManager(30 * time.Second)
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				expired := custodyMgr.Reap()
				if expired > 0 {
					metrics.CustodyExpiredTotal.Add(float64(expired))
					slog.Debug("custody: reaped expired offers", "count", expired)
				}
				metrics.CustodyPending.Set(float64(custodyMgr.PendingCount()))
				custodyMgr.Clear()
			}
		}
	}()

	// Reticulum packet handler — processes announces, path requests, protocol
	// enhancement packets (MESHSAT-407), and forwards data packets.
	reticulumPacketHandler := func(iface reticulum.InterfaceType, raw []byte) {
		// Try to parse as announce to update routing table.
		if ann, err := reticulum.UnmarshalAnnouncePacket(raw); err == nil {
			if reticulumRouter.ProcessAnnounce(ann, iface) {
				// Flood announce to all other interfaces (Reticulum transport behavior).
				// This ensures TCP-connected RNS nodes learn about MQTT-connected bridges
				// and vice versa.
				reticulumRelay.Broadcast(ctx, iface, raw)
			}
			return
		}
		// Check if this is a path request — Hub responds with routing info.
		if reticulumPathHandler.HandlePacket(ctx, iface, raw) {
			return
		}
		// Dispatch protocol enhancement packets (MESHSAT-407).
		if len(raw) > 0 {
			switch raw[0] {
			case reticulum.BridgeTimeSyncReq:
				// Bridge asking for time — respond with Hub's NTP-authoritative time.
				slog.Debug("timesync: received request", "from", iface)
				resp := timesync.BuildTimeSyncResponse(raw, hubTimeService)
				if resp != nil {
					// Broadcast response — requesting bridge matches by dest hash.
					reticulumRelay.Broadcast(ctx, iface, resp)
				}
				return
			case reticulum.BridgeTimeSyncResp:
				slog.Debug("timesync: received response (hub ignores — we are authority)")
				return
			case reticulum.BridgeCustodyOffer:
				offer, err := protocol.UnmarshalCustodyOffer(raw)
				if err != nil {
					slog.Warn("custody: malformed offer", "from", iface, "error", err)
					return
				}
				if hubIdentity == nil || !hubIdentity.IsLoaded() {
					slog.Warn("custody: cannot accept — identity not loaded")
					return
				}
				destHash := hubIdentity.DestHash()
				var acceptorHash [16]byte
				copy(acceptorHash[:], destHash[:])
				privKey := ed25519.PrivateKey(hubIdentity.Identity().SigningPrivateBytes())
				ack := protocol.SignCustodyACK(offer.CustodyID, acceptorHash, privKey)
				ackData := protocol.MarshalCustodyACK(ack)
				if err := reticulumRelay.SendVia(ctx, iface, ackData); err != nil {
					slog.Warn("custody: failed to send ACK", "from", iface, "error", err)
				} else {
					metrics.CustodyAcceptedTotal.Inc()
					slog.Info("custody: accepted offer",
						"custody_id", hex.EncodeToString(offer.CustodyID[:]),
						"source", hex.EncodeToString(offer.SourceHash[:]),
						"delivery_id", offer.DeliveryID,
						"from", iface,
					)
				}
				return
			case reticulum.BridgeCustodyACK:
				ack, err := protocol.UnmarshalCustodyACK(raw)
				if err != nil {
					slog.Warn("custody: malformed ACK", "from", iface, "error", err)
					return
				}
				if custodyMgr.HandleACK(ack) {
					slog.Info("custody: ACK matched pending offer",
						"custody_id", hex.EncodeToString(ack.CustodyID[:]),
					)
				}
				return
			}
		}
		// Otherwise, attempt to relay the packet.
		if err := reticulumRelay.Forward(ctx, iface, raw); err != nil {
			slog.Debug("reticulum: relay drop", "from", iface, "error", err)
		}
	}

	// Bridge bus.MessageBus → reticulum.MQTTPublisher (adapts named handler type).
	mqttBridge := reticulum.NewMQTTBridge(
		msgBus.Publish,
		func(topic string, qos byte, handler func(string, []byte)) error {
			return msgBus.Subscribe(topic, qos, bus.MessageHandler(handler))
		},
		msgBus.IsConnected,
	)

	// Reticulum transport interfaces.
	retMQTTIface := reticulum.NewMQTTInterface(mqttBridge)
	retMQTTIface.SetHandler(reticulumPacketHandler)
	reticulumRelay.RegisterInterface(retMQTTIface)

	// Reticulum TCP interface — external RNS nodes connect via HDLC framing.
	// stunnel on DMZ terminates TLS; raw HDLC arrives here on port 4242.
	var retTCPIface *reticulum.TCPInterface
	if cfg.ReticulumTCPEnabled && cfg.ReticulumTCPAddr != "" {
		retTCPIface = reticulum.NewTCPInterface(cfg.ReticulumTCPAddr)
		retTCPIface.SetHandler(reticulumPacketHandler)
		reticulumRelay.RegisterInterface(retTCPIface)
		if err := retTCPIface.Start(); err != nil {
			slog.Error("reticulum: tcp interface failed to start", "error", err)
		} else {
			slog.Info("reticulum: tcp interface started", "addr", cfg.ReticulumTCPAddr)
			defer retTCPIface.Stop()
		}
	}

	// Reticulum transport interfaces — satellite backends.
	// These are registered now; webhook handlers wire SetReticulumIface later.
	var retIridiumIface *reticulum.IridiumInterface
	if cloudloopClient != nil {
		iridiumBackend := constellation.NewIridiumBackend(cloudloopClient)
		retIridiumIface = reticulum.NewIridiumInterface(reticulum.NewBackendAdapter(
			func(ctx2 context.Context, deviceID string, payload []byte) error {
				_, err2 := iridiumBackend.Send(ctx2, deviceID, payload)
				return err2
			},
			iridiumBackend.IsAvailable,
			iridiumBackend.MaxPayload(),
			iridiumBackend.CostPerMessage(),
		))
		retIridiumIface.SetHandler(reticulumPacketHandler)
		reticulumRelay.RegisterInterface(retIridiumIface)
	}

	var retGlobalstarIface *reticulum.GlobalstarInterface
	if globalstarClient != nil {
		globalstarBackend := constellation.NewGlobalstarBackend(globalstarClient)
		retGlobalstarIface = reticulum.NewGlobalstarInterface(reticulum.NewBackendAdapter(
			func(ctx2 context.Context, deviceID string, payload []byte) error {
				_, err2 := globalstarBackend.Send(ctx2, deviceID, payload)
				return err2
			},
			globalstarBackend.IsAvailable,
			globalstarBackend.MaxPayload(),
			globalstarBackend.CostPerMessage(),
		))
		retGlobalstarIface.SetHandler(reticulumPacketHandler)
		reticulumRelay.RegisterInterface(retGlobalstarIface)
	}

	// SMS as Reticulum interface (RX-only, inbound via Twilio webhook). [MESHSAT-446]
	retSMSIface := reticulum.NewSMSInterface()
	retSMSIface.SetHandler(reticulumPacketHandler)
	reticulumRelay.RegisterInterface(retSMSIface)

	// Tor as Reticulum interface (proxied via MQTT).
	torOnion := os.Getenv("HUB_TOR_ONION")
	if torOnion != "" {
		retTorIface := reticulum.NewTorInterface(torOnion, mqttBridge)
		retTorIface.SetHandler(reticulumPacketHandler)
		reticulumRelay.RegisterInterface(retTorIface)
	}

	// WireGuard as Reticulum interface (proxied via MQTT).
	if cfg.WGEnabled {
		retWGIface := reticulum.NewWireGuardInterface(true, mqttBridge)
		retWGIface.SetHandler(reticulumPacketHandler)
		reticulumRelay.RegisterInterface(retWGIface)
	}

	// Start Reticulum MQTT interface + route expiry goroutine.
	if msgBus.IsConnected() {
		if err := retMQTTIface.Start(); err != nil {
			slog.Error("reticulum: failed to start mqtt interface", "error", err)
		} else {
			slog.Info("reticulum: mqtt interface started", "topic", reticulum.ReticulumMQTTTopic)
		}
	}
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reticulumRouter.ExpireStale()
				reticulumPathHandler.PruneStale()
			}
		}
	}()

	// Reticulum route hint publisher — broadcasts routing table to bridges via MQTT.
	reticulumHintPublisher := reticulum.NewRouteHintPublisher(
		reticulumRouter, mqttBridge, hubIdentity,
		reticulum.DefaultRouteHintPublisherConfig(),
	)
	go reticulumHintPublisher.Run(ctx)

	// Reticulum TCP announce — periodically announce Hub identity to connected
	// TCP clients (RNS nodes). Without this, RNS nodes can't discover the Hub.
	if retTCPIface != nil && hubIdentity != nil && hubIdentity.IsLoaded() {
		go func() {
			// Initial delay for connections to establish.
			time.Sleep(5 * time.Second)
			announceToTCP := func() {
				if retTCPIface.ClientCount() == 0 {
					return
				}
				ann, err := reticulum.NewAnnounce(hubIdentity.Identity(), hubIdentity.AppName(), nil)
				if err != nil {
					slog.Error("reticulum: tcp announce failed", "error", err)
					return
				}
				pkt := ann.MarshalPacket()
				if err := retTCPIface.Send(ctx, "", pkt); err != nil {
					slog.Debug("reticulum: tcp announce send failed", "error", err)
				} else {
					slog.Info("reticulum: announced hub identity to tcp clients",
						"dest", hubIdentity.DestHashHex(), "clients", retTCPIface.ClientCount())
				}
			}

			announceToTCP()
			ticker := time.NewTicker(60 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					announceToTCP()
				}
			}
		}()
	}

	// MSVQ-SC decoder (for Android-compressed messages).
	var msvqscDecoder *hubmsvqsc.Decoder
	msvqscCBPath := os.Getenv("HUB_MSVQSC_CODEBOOK")
	msvqscCIPath := os.Getenv("HUB_MSVQSC_CORPUS")
	if msvqscCBPath == "" {
		msvqscCBPath = "/data/msvqsc/codebook_v1.bin"
	}
	if msvqscCIPath == "" {
		msvqscCIPath = "/data/msvqsc/corpus_index.bin"
	}
	if d, err := hubmsvqsc.Load(msvqscCBPath, msvqscCIPath); err == nil {
		msvqscDecoder = d
		stages, k, dim, corpus := d.Stats()
		slog.Info("msvqsc: decoder loaded", "stages", stages, "k", k, "dim", dim, "corpus", corpus)
	} else {
		slog.Info("msvqsc: decoder not available (Android compression won't decode)", "error", err)
	}

	// RockBLOCK webhook handler.
	rbHandler := rockblock.NewHandler(msgBus, cfg.RockBLOCKSecret)
	rbHandler.SetAudit(auditSvc)
	rbHandler.SetDedup(dedupTracker)
	rbHandler.SetReassembler(reassembler)
	rbHandler.SetKeyStore(keyStore)
	rbHandler.SetDeadman(deadmanMonitor)
	rbHandler.SetMSVQSC(msvqscDecoder)
	rbHandler.SetStore(dataStore)
	rbHandler.SetHeMBReassembler(hembReassemblyBuf)

	// Rock7 MT sender (for sending messages to devices via Iridium).
	var rock7Client *rock7.Client
	if cfg.Rock7Username != "" {
		rock7Client = rock7.NewClient(cfg.Rock7Username, cfg.Rock7Password)
		slog.Info("rock7: MT sender enabled", "username", cfg.Rock7Username)
	}

	// Globalstar MO webhook handler.
	gsHandler := globalstar.NewHandler(msgBus, cfg.GlobalstarWebhookSecret)
	gsHandler.SetAudit(auditSvc)
	gsHandler.SetDedup(dedupTracker)
	gsHandler.SetReassembler(reassembler)
	gsHandler.SetKeyStore(keyStore)
	gsHandler.SetDeadman(deadmanMonitor)
	gsHandler.SetMSVQSC(msvqscDecoder)

	// Cloudloop LingoMO webhook handler.
	clHandler := cloudloop.NewWebhookHandler(msgBus)
	clHandler.SetAudit(auditSvc)
	clHandler.SetDedup(dedupTracker)
	clHandler.SetReassembler(reassembler)
	clHandler.SetKeyStore(keyStore)
	clHandler.SetDeadman(deadmanMonitor)
	clHandler.SetMSVQSC(msvqscDecoder)
	clHandler.SetStore(dataStore)
	clHandler.SetHeMBReassembler(hembReassemblyBuf)
	clHandler.SetResolver(thingResolver)
	if cfg.CloudloopWebhookAllowedIPs != "" {
		clHandler.SetAllowedIPs(strings.Split(cfg.CloudloopWebhookAllowedIPs, ","))
	}

	// Wire Reticulum interfaces to webhook handlers for inbound packet detection.
	if retIridiumIface != nil {
		rbHandler.SetReticulumIface(retIridiumIface)
		clHandler.SetReticulumIface(retIridiumIface)
	}
	if retGlobalstarIface != nil {
		gsHandler.SetReticulumIface(retGlobalstarIface)
	}

	// Cloudloop MQTT subscriber (receives LingoMO messages from Cloudloop's MQTT broker).
	if cfg.CloudloopAccountID != "" && cfg.CloudloopMQTTBroker != "" {
		clMQTTCfg := cloudloop.MQTTSubscriberConfig{
			BrokerURL:  cfg.CloudloopMQTTBroker,
			CACertFile: cfg.CloudloopMQTTCACert,
			CertFile:   cfg.CloudloopMQTTCert,
			KeyFile:    cfg.CloudloopMQTTKey,
			AccountID:  cfg.CloudloopAccountID,
		}
		clMQTTSub := cloudloop.NewMQTTSubscriber(clMQTTCfg, clHandler.ProcessLingoMO)
		if err := clMQTTSub.Start(context.Background()); err != nil {
			slog.Error("cloudloop mqtt: failed to start subscriber", "error", err)
		} else {
			slog.Info("cloudloop mqtt: subscriber started",
				"broker", cfg.CloudloopMQTTBroker,
				"account", cfg.CloudloopAccountID,
			)
			defer clMQTTSub.Stop()
		}
	}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)
	r.Use(hubmw.RequestID)             // Assign/propagate correlation IDs.
	r.Use(hubmw.MaxBodySize(10 << 20)) // 10MB global request body limit (webhook handlers override).
	r.Use(api.SecurityHeaders)
	r.Use(api.WSTokenFromQuery) // Copy ?token= query param to Authorization header for WebSocket clients.

	// Bounded worker for async API key last_used updates (avoids unbounded goroutines).
	touchCh := make(chan string, 64)
	go func() {
		for id := range touchCh {
			_ = dataStore.TouchAPIKeyLastUsed(context.Background(), id)
		}
	}()

	// API key middleware — runs first; if bearer is a meshsat_ key, validates and sets
	// user+tenant in context. Non-API-key tokens pass through to JWT/token middleware.
	apiKeyValidator := func(ctx context.Context, keyHash string) (*hubauth.User, string, error) {
		k, tenantID, err := dataStore.GetAPIKeyByHash(ctx, keyHash)
		if err != nil {
			return nil, "", err
		}
		// Touch last_used via bounded worker (non-blocking, drops if full).
		select {
		case touchCh <- k.ID:
		default:
		}
		user := &hubauth.User{
			ID:        "apikey:" + k.ID,
			Name:      k.Label,
			Roles:     []string{k.Role},
			TenantID:  "", // will be set from tenantID below
			ExpiresAt: k.ExpiresAt,
		}
		return user, tenantID, nil
	}
	r.Use(hubauth.APIKeyMiddleware(apiKeyValidator))

	// Auth middleware — auto-detect mode if not explicitly set
	authMode := cfg.AuthMode
	if authMode == "" {
		if cfg.OIDCIssuerURL != "" {
			authMode = "oidc"
		} else if cfg.JWTSigningKey != "" {
			authMode = "local"
		} else if cfg.AuthToken != "" {
			authMode = "token"
		} else {
			authMode = "none"
		}
	}
	var jwtSecret []byte
	if authMode == "local" {
		if len(cfg.JWTSigningKey) < 32 {
			slog.Error("auth: HUB_JWT_SIGNING_KEY must be at least 32 characters for local auth mode")
			os.Exit(1)
		}
		jwtSecret = []byte(cfg.JWTSigningKey)
	}
	r.Use(hubauth.Middleware(hubauth.Config{
		Mode:              authMode,
		Token:             cfg.AuthToken,
		OIDCIssuerURL:     cfg.OIDCIssuerURL,
		OIDCAudience:      cfg.OIDCAudience,
		JWTSecret:         jwtSecret,
		OIDCCertPin:       cfg.OIDCCertPin,
		OIDCCertPinBackup: cfg.OIDCCertPinBackup,
	}))
	// Tenant isolation middleware — resolves tenant from JWT claim / X-Tenant-ID header / default.
	// Enforce mode disabled for backward compatibility; enable via HUB_TENANT_ENFORCE=true.
	r.Use(hubauth.TenantMiddleware(cfg.TenantEnforce))
	r.Use(metrics.ChiMiddleware)
	r.Use(hubmw.Logging) // Structured HTTP request logging (runs last to see auth context).

	r.Get("/healthz", health.LivezHandler)
	r.Get("/readyz", checker.ReadyzHandler)
	r.Get("/startupz", checker.StartupzHandler)
	r.Handle("/metrics", metrics.Handler())

	// pprof profiling endpoints (opt-in, behind auth).
	if cfg.PprofEnabled {
		slog.Warn("pprof endpoints enabled at /debug/pprof/ — ensure auth is configured")
		r.HandleFunc("/debug/pprof/", http.DefaultServeMux.ServeHTTP)
		r.HandleFunc("/debug/pprof/{profile}", http.DefaultServeMux.ServeHTTP)
	}

	// WebSocket real-time event hub
	wsHub := api.NewWSHub()
	r.Get("/api/ws", wsHub.HandleWS)
	// Bridge MQTT events to WebSocket for live dashboard updates
	if msgBus.IsConnected() {
		for _, topic := range []string{"meshsat/+/mo/decoded", "meshsat/+/position", "meshsat/+/sos"} {
			t := topic
			_ = msgBus.Subscribe(t, 0, func(topic string, payload []byte) {
				wsHub.Broadcast(payload)
			})
		}
	}
	// Webhook endpoints — rate limited to 60 requests/minute per source IP.
	r.Post("/api/webhook/rockblock", hubmw.WebhookRateLimit(http.HandlerFunc(rbHandler.ServeHTTP), 60).ServeHTTP)
	r.Post("/api/webhook/globalstar", hubmw.WebhookRateLimit(http.HandlerFunc(gsHandler.ServeHTTP), 60).ServeHTTP)
	r.Post("/api/webhook/cloudloop", hubmw.WebhookRateLimit(http.HandlerFunc(clHandler.ServeHTTP), 60).ServeHTTP)

	// QR provision claim — unauthenticated (nonce IS the auth, single-use, 30min TTL).
	provisionClaimHandler := api.NewBridgeProvisionHandler(dataStore, bridgeCA, directoryTrustAnchor)
	r.Get("/api/bridges/{id}/provision/{nonce}", provisionClaimHandler.ClaimProvision)

	// SMS gateway (optional — inbound webhook + outbound subscriber + send API)
	var smsClientForSend *sms.Client
	if cfg.SMSEnabled && cfg.SMSAccountSID != "" {
		var smsClient *sms.Client
		if cfg.SMSAPIKeySID != "" {
			smsClient = sms.NewClientWithAPIKey(cfg.SMSAccountSID, cfg.SMSAPIKeySID, cfg.SMSAuthToken, cfg.SMSFromNumber)
			slog.Info("sms: using API key auth", "key_sid", cfg.SMSAPIKeySID)
		} else {
			smsClient = sms.NewClient(cfg.SMSAccountSID, cfg.SMSAuthToken, cfg.SMSFromNumber)
		}
		smsClientForSend = smsClient
		smsWebhook := sms.NewWebhookHandler(msgBus, cfg.SMSWebhookSecret)
		smsWebhook.SetStore(dataStore)
		smsWebhook.SetKeyStore(keyStore)
		// [MESHSAT-446] Wire full pipeline (parity with Rock7/Cloudloop)
		smsWebhook.SetDedup(dedupTracker)
		smsWebhook.SetReassembler(reassembler)
		smsWebhook.SetMSVQSC(msvqscDecoder)
		smsWebhook.SetDeadman(deadmanMonitor)
		smsWebhook.SetAudit(auditSvc)
		smsWebhook.SetHeMBReassembler(hembReassemblyBuf)
		// SMS Reticulum interface deferred to MESHSAT-404
		r.Post("/api/webhook/sms", smsWebhook.ServeHTTP)
		if msgBus.IsConnected() {
			smsSub := sms.NewSubscriber(smsClient, msgBus)
			if err := smsSub.Start(); err != nil {
				slog.Error("sms: failed to start outbound subscriber", "error", err)
			} else {
				slog.Info("sms: gateway enabled", "from", cfg.SMSFromNumber)
			}
		}
	}

	// SMS inbound relay — Android publishes SMS to MQTT, Hub persists them.
	if msgBus.IsConnected() {
		smsInSub := sms.NewInboundSubscriber(msgBus, dataStore, store.DefaultTenantID)
		smsInSub.SetKeyStore(keyStore)
		if err := smsInSub.Start(); err != nil {
			slog.Error("sms: failed to start inbound MQTT subscriber", "error", err)
		} else {
			slog.Info("sms: inbound MQTT relay enabled (meshsat/+/sms/inbound)")
		}
	}

	// Email gateway routes (PGP key management + inbound webhook)
	if emailKeyRing != nil {
		emailWebhook := hubemail.NewWebhookHandler(msgBus, emailKeyRing)
		r.Post("/api/webhook/email", emailWebhook.ServeHTTP)

		emailAPIHandler := hubemail.NewAPIHandler(emailKeyRing)
		r.Get("/api/email/keys/public", emailAPIHandler.GetPublicKey)
		r.Get("/api/email/keys", emailAPIHandler.ListContacts)
		r.Post("/api/email/keys", emailAPIHandler.AddContact)
		r.Delete("/api/email/keys/{email}", emailAPIHandler.DeleteContact)
		r.Post("/api/email/test", emailAPIHandler.TestSend)
		emailAPIHandler.SetClient(hubemail.NewClient(cfg.EmailSMTPHost, cfg.EmailFrom, cfg.EmailUsername, cfg.EmailPassword, emailKeyRing))
	}

	// Auth info
	r.Get("/api/auth/me", api.AuthMeHandler)

	// Local auth endpoints (login/refresh/logout — exempt from auth middleware)
	if authMode == "local" {
		sessionMgr := hubauth.NewSessionManager(jwtSecret, "meshsat-hub")
		loginHandler := api.NewLoginHandler(dataStore, sessionMgr, auditSvc)
		r.Post("/api/auth/login", loginHandler.Login)
		r.Post("/api/auth/refresh", loginHandler.Refresh)
		r.Post("/api/auth/logout", loginHandler.Logout)

		// User management (owner-only)
		userHandler := api.NewUserHandler(dataStore)
		r.Route("/api/users", func(r chi.Router) {
			r.Use(hubauth.RequireRole(hubauth.RoleOwner))
			r.Get("/", userHandler.ListUsers)
			r.Post("/", userHandler.CreateUser)
			r.Get("/{id}", userHandler.GetUser)
			r.Put("/{id}", userHandler.UpdateUser)
			r.Delete("/{id}", userHandler.DeleteUser)
		})
	}

	// API key management (owner-only)
	apiKeyHandler := api.NewAPIKeyHandler(dataStore)
	r.Route("/api/auth/keys", func(r chi.Router) {
		r.Use(hubauth.RequireRole(hubauth.RoleOwner))
		r.Post("/", apiKeyHandler.CreateKey)
		r.Get("/", apiKeyHandler.ListKeys)
		r.Delete("/{id}", apiKeyHandler.DeleteKey)
	})

	// Secret rotation (owner-only)
	rotationHandler := api.NewRotationHandler(dataStore, bridgeCommander)
	r.Route("/api/auth/keys/{id}/rotate", func(r chi.Router) {
		r.Use(hubauth.RequireRole(hubauth.RoleOwner))
		r.Post("/", rotationHandler.RotateAPIKey)
	})
	r.Route("/api/bridges/{id}/credentials/rotate", func(r chi.Router) {
		r.Use(hubauth.RequireRole(hubauth.RoleOwner))
		r.Post("/", rotationHandler.RotateBridgeCredentials)
	})

	// Credential management (MESHSAT-356)
	credMasterKey := bootstrapCredentialMasterKey(dataStore)
	credHandler := api.NewCredentialHandler(dataStore, credMasterKey)
	if bridgeCommander != nil {
		credHandler.SetCommander(bridgeCommander)
	}
	r.Route("/api/credentials", func(r chi.Router) {
		r.Use(hubauth.RequireRole(hubauth.RoleOperator))
		r.Post("/upload", credHandler.Upload)
		r.Get("/", credHandler.List)
		r.Get("/expiry", credHandler.ListExpiring)
		r.Get("/{id}", credHandler.Get)
		r.Delete("/{id}", credHandler.Delete)
		r.Post("/{id}/distribute", credHandler.Distribute)
	})

	// Bridge registry API
	bridgeHandler := api.NewBridgeHandler(dataStore, msgBus)
	r.Get("/api/bridges", bridgeHandler.ListBridges)
	r.Post("/api/bridges", bridgeHandler.CreateBridge)
	r.Get("/api/bridges/{id}", bridgeHandler.GetBridge)
	r.Put("/api/bridges/{id}", bridgeHandler.UpdateBridge)
	r.Delete("/api/bridges/{id}", bridgeHandler.DeleteBridge)
	if bridgeCommander != nil {
		bridgeCmdHandler := api.NewBridgeCommandHandler(dataStore, bridgeCommander)
		r.Post("/api/bridges/{id}/command", bridgeCmdHandler.SendCommand)
	}

	// Bridge MQTT authentication API
	bridgeAuthHandler := api.NewBridgeAuthHandler(dataStore, bridgeCA)
	r.Post("/api/bridges/{id}/credentials", bridgeAuthHandler.GenerateCredentials)
	r.Post("/api/bridges/{id}/certificate", bridgeAuthHandler.IssueCertificate)
	r.Post("/api/bridges/acl/regenerate", bridgeAuthHandler.RegenerateACL)

	// One-step bridge provisioning with QR code (MESHSAT-414)
	provisionHandler := api.NewBridgeProvisionHandler(dataStore, bridgeCA, directoryTrustAnchor)
	r.Post("/api/bridges/{id}/provision", provisionHandler.Provision)
	r.Post("/api/bridges/{id}/provision/qr", provisionHandler.ProvisionQR)

	// Directory REST — tenant-scoped contacts + signed snapshot
	// [MESHSAT-538]. Opens a dedicated *sql.DB connection on the
	// same SQLite file so the directory package can own its
	// transactions without threading through the dbwrap interface
	// (which doesn't expose BeginTx). Only wired in standalone mode
	// today; cluster/MariaDB support lands in a follow-up once a
	// MariaDB-compatible SQLStore is added.
	if cfg.Mode == "" || cfg.Mode == "standalone" {
		directoryRawDB, err := sql.Open("sqlite", "file:/data/hub.db?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=ON&_synchronous=NORMAL")
		if err != nil {
			slog.Error("directory: open raw sqlite failed", "error", err)
		} else {
			directoryRawDB.SetMaxOpenConns(1)
			directorySQLStore := directory.NewSQLStore(directoryRawDB)
			if err := directorySQLStore.Migrate(ctx); err != nil {
				slog.Error("directory: migrate failed", "error", err)
			} else {
				directoryHandler := api.NewDirectoryHandler(directorySQLStore, directoryTrustAnchor)
				r.Get("/api/v1/directory/contacts", directoryHandler.ListContacts)
				r.Post("/api/v1/directory/contacts", directoryHandler.CreateContact)
				r.Get("/api/v1/directory/contacts/{id}", directoryHandler.GetContact)
				r.Put("/api/v1/directory/contacts/{id}", directoryHandler.UpdateContact)
				r.Delete("/api/v1/directory/contacts/{id}", directoryHandler.DeleteContact)
				r.Get("/api/v1/directory/snapshot", directoryHandler.GetSnapshot)
				slog.Info("directory REST registered")
			}
		}
	}

	// HeMB bond group management (MESHSAT-487)
	bondGroupHandler := api.NewBondGroupHandler(dataStore, msgBus)
	r.Get("/api/bridges/{bridgeID}/bond-groups", bondGroupHandler.ListBondGroups)
	r.Post("/api/bridges/{bridgeID}/bond-groups", bondGroupHandler.CreateBondGroup)
	r.Get("/api/bridges/{bridgeID}/bond-groups/{groupID}", bondGroupHandler.GetBondGroup)
	r.Put("/api/bridges/{bridgeID}/bond-groups/{groupID}", bondGroupHandler.UpdateBondGroup)
	r.Delete("/api/bridges/{bridgeID}/bond-groups/{groupID}", bondGroupHandler.DeleteBondGroup)

	// HeMB reassembly stats (MESHSAT-489)
	if hembReassemblyBuf != nil {
		hembStatsHandler := api.NewHeMBStatsHandler(hembReassemblyBuf)
		r.Get("/api/hemb/stats", hembStatsHandler.GetStats)
	}

	// Platform settings (MQTT public URL for bridge onboarding)
	r.Get("/api/settings/mqtt-url", bridgeAuthHandler.GetMQTTURL)
	r.Put("/api/settings/mqtt-url", bridgeAuthHandler.SetMQTTURL)

	// Service security status + password rotation
	securityHandler := api.NewSecuritySettingsHandler(dataStore)
	r.Get("/api/settings/security", securityHandler.GetSecurityStatus)
	r.Post("/api/settings/security/rotate", securityHandler.RotateServicePasswords)

	// Device registry API
	deviceHandler := api.NewDeviceHandler(dataStore)
	r.Get("/api/devices", deviceHandler.ListDevices)
	r.Post("/api/devices", deviceHandler.CreateDevice)
	r.Get("/api/devices/{imei}", deviceHandler.GetDevice)
	r.Put("/api/devices/{imei}", deviceHandler.UpdateDevice)
	r.Delete("/api/devices/{imei}", deviceHandler.DeleteDevice)

	// Device groups API (MESHSAT-311)
	groupHandler := api.NewDeviceGroupHandler(dataStore)
	r.Get("/api/device-groups", groupHandler.ListGroups)
	r.Post("/api/device-groups", groupHandler.CreateGroup)
	r.Get("/api/device-groups/{id}", groupHandler.GetGroup)
	r.Put("/api/device-groups/{id}", groupHandler.UpdateGroup)
	r.Delete("/api/device-groups/{id}", groupHandler.DeleteGroup)
	r.Post("/api/device-groups/{id}/members", groupHandler.AddMember)
	r.Delete("/api/device-groups/{id}/members/{imei}", groupHandler.RemoveMember)
	r.Get("/api/device-groups/{id}/devices", groupHandler.ListDevices)

	// Device config versioning
	configHandler := api.NewDeviceConfigHandler(dataStore)
	r.Get("/api/devices/{imei}/config", configHandler.GetLatest)
	r.Put("/api/devices/{imei}/config", configHandler.CreateVersion)
	r.Get("/api/devices/{imei}/config/history", configHandler.ListVersions)
	r.Get("/api/devices/{imei}/config/{version}", configHandler.GetVersion)

	// Device encryption key management
	deviceKeyHandler := api.NewDeviceKeyHandler(dataStore, keyStore, bridgeCommander)
	r.Post("/api/devices/{imei}/keys", deviceKeyHandler.CreateKey)
	r.Post("/api/devices/{imei}/keys/import", deviceKeyHandler.ImportKey)
	r.Get("/api/devices/{imei}/keys", deviceKeyHandler.ListKeys)
	r.Delete("/api/devices/{imei}/keys/{id}", deviceKeyHandler.DeleteKey)
	r.Post("/api/devices/{imei}/keys/rotate", deviceKeyHandler.RotateAndDistribute)
	r.Post("/api/devices/{imei}/keys/distribute", deviceKeyHandler.DistributeKey)

	// Channel key rotation — Hub generates + distributes to all bridges [MESHSAT-447]
	channelKeyHandler := api.NewChannelKeyHandler(dataStore, keyStore, bridgeCommander)
	r.Post("/api/keys/channel/rotate", channelKeyHandler.RotateChannelKey)

	// MT message send (Rock7 / Iridium)
	sendHandler := api.NewSendHandler(rock7Client, dataStore)
	sendHandler.SetKeyStore(keyStore)
	sendHandler.SetSMSClient(smsClientForSend)
	r.Post("/api/devices/{imei}/send", sendHandler.SendMessage)
	r.Post("/api/sms/send", sendHandler.SendSMS)

	// Message history API
	messageHandler := api.NewMessageHandler(dataStore)
	r.Get("/api/messages", messageHandler.ListMessages)
	r.Get("/api/messages/{id}", messageHandler.GetMessage)

	// Position API (for map)
	positionHandler := api.NewPositionHandler(dataStore)
	r.Get("/api/positions/latest", positionHandler.AllLatestPositions)
	r.Get("/api/devices/{imei}/position", positionHandler.LatestPosition)
	r.Get("/api/devices/{imei}/positions", positionHandler.ListPositions)
	r.Get("/api/ratelimit", rateLimitHandler.GetAllUsage)
	r.Get("/api/ratelimit/{deviceID}", rateLimitHandler.GetUsage)
	r.Post("/api/ratelimit/{deviceID}/override", rateLimitHandler.PostOverride)
	r.Delete("/api/ratelimit/{deviceID}/override", rateLimitHandler.DeleteOverride)
	r.Get("/api/webhooks", webhookAPIHandler.ListWebhooks)
	r.Post("/api/webhooks", webhookAPIHandler.CreateWebhook)
	r.Delete("/api/webhooks/{id}", webhookAPIHandler.DeleteWebhook)
	r.Get("/api/webhooks/logs", webhookAPIHandler.GetLogs)
	// MPTCP concentrator API
	mptcpHandler := mptcp.NewAPIHandler(mptcpMonitor)
	r.Get("/api/mptcp/status", mptcpHandler.GetStatus)
	r.Put("/api/mptcp/strategy", mptcpHandler.SetStrategy)
	r.Get("/api/mptcp/endpoints", mptcpHandler.ListEndpoints)
	r.Post("/api/mptcp/endpoints", mptcpHandler.AddEndpointHandler)
	r.Delete("/api/mptcp/endpoints/{id}", mptcpHandler.RemoveEndpointHandler)

	// Cluster health management (available in any mode — standalone returns basic info)
	if clusterMonitor == nil {
		clusterMonitor = cluster.NewMonitor(nil, cfg.MQTTClientID, "", nil)
	}
	clusterHandler := cluster.NewAPIHandler(clusterMonitor)
	r.Get("/api/cluster/node", clusterHandler.GetNodeStatus)
	r.Get("/api/cluster/status", clusterHandler.GetClusterStatus)
	r.Get("/api/cluster/actions", clusterHandler.GetActions)
	r.Post("/api/cluster/actions/{id}", clusterHandler.ExecuteAction)
	r.Put("/api/cluster/peers", clusterHandler.SetPeers)

	// Integration channel status API
	integrationHandler := api.NewIntegrationHandler(cfg)
	integrationHandler.SetFederationGetter(func() api.FederationStatter {
		if takFederation == nil {
			return nil
		}
		return takFederation
	})
	r.Get("/api/integrations", integrationHandler.ListIntegrations)
	r.Get("/api/tak/federation/peers", integrationHandler.ListFederationPeers)
	r.Get("/api/tak/missions", func(w http.ResponseWriter, r *http.Request) {
		if cfg.TAKHost == "" {
			api.WriteJSON(w, http.StatusOK, []interface{}{})
			return
		}
		proxy := tak.NewMartiProxy(cfg.TAKHost, 8443, true)
		missions, err := proxy.ListMissions()
		if err != nil {
			api.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		api.WriteJSON(w, http.StatusOK, missions)
	})
	r.Get("/api/tak/fleet-status", func(w http.ResponseWriter, r *http.Request) {
		fedIn, fedOut, fedPeers := int64(0), int64(0), 0
		if integrationHandler.GetFederation() != nil {
			fedIn, fedOut, fedPeers = integrationHandler.GetFederation().Stats()
		}
		api.WriteJSON(w, http.StatusOK, map[string]interface{}{
			"tak_enabled":            true, // Hub is always a TAK/CoT gateway
			"tak_host":               cfg.TAKHost,
			"external_tak_connected": cfg.TAKEnabled && cfg.TAKHost != "",
			"federation_enabled":     cfg.TAKFederationEnabled,
			"federation_peers":       fedPeers,
			"federation_in":          fedIn,
			"federation_out":         fedOut,
			"mode":                   "Hub CoT Gateway",
		})
	})

	r.Get("/api/constellations", func(w http.ResponseWriter, r *http.Request) {
		backends := constellationRouter.ListBackends()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"backends": backends})
	})
	r.Get("/api/credits", func(w http.ResponseWriter, r *http.Request) {
		balance, err := cloudloopClient.GetCreditBalance(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(balance)
	})

	// Cost tracking ledger
	costsHandler := api.NewCostsHandler(dataStore)
	r.Get("/api/costs", costsHandler.ListCosts)
	r.Get("/api/costs/summary", costsHandler.Summary)

	// Notification preferences (per-device Apprise URLs)
	notifHandler := api.NewNotificationHandler(dataStore)
	r.Get("/api/notifications/prefs", notifHandler.ListPrefs)
	r.Get("/api/notifications/prefs/{device_imei}", notifHandler.GetPref)
	r.Put("/api/notifications/prefs/{device_imei}", notifHandler.SavePref)
	r.Delete("/api/notifications/prefs/{device_imei}", notifHandler.DeletePref)

	// Escalation chains and alerts
	escHandler := api.NewEscalationHandler(dataStore, escEngine)
	r.Get("/api/escalation/chains", escHandler.ListChains)
	r.Post("/api/escalation/chains", escHandler.CreateChain)
	r.Get("/api/escalation/chains/{id}", escHandler.GetChain)
	r.Delete("/api/escalation/chains/{id}", escHandler.DeleteChain)
	r.Get("/api/alerts", escHandler.ListAlerts)
	r.Post("/api/alerts", escHandler.TriggerAlert)
	r.Get("/api/alerts/{id}", escHandler.GetAlert)
	r.Post("/api/alerts/{id}/ack", escHandler.AcknowledgeAlert)

	// Alert rules (configurable alerting engine, MESHSAT-313)
	alertRuleHandler := api.NewAlertRuleHandler(dataStore)
	r.Get("/api/alert-rules", alertRuleHandler.ListAlertRules)
	r.Post("/api/alert-rules", alertRuleHandler.CreateAlertRule)
	r.Get("/api/alert-rules/{id}", alertRuleHandler.GetAlertRule)
	r.Put("/api/alert-rules/{id}", alertRuleHandler.UpdateAlertRule)
	r.Delete("/api/alert-rules/{id}", alertRuleHandler.DeleteAlertRule)

	// Dead man's switch API
	deadmanHandler := api.NewDeadmanHandler(deadmanMonitor)
	r.Get("/api/deadman", deadmanHandler.ListConfigs)
	r.Put("/api/deadman/{imei}", deadmanHandler.Configure)
	r.Delete("/api/deadman/{imei}", deadmanHandler.Delete)
	r.Post("/api/deadman/{imei}/snooze", deadmanHandler.Snooze)

	// Audit log (owner-only)
	auditHandler := api.NewAuditHandler(auditSvc)
	r.Route("/api/audit", func(r chi.Router) {
		r.Use(hubauth.RequireRole(hubauth.RoleOwner))
		r.Get("/", auditHandler.ListEntries)
		r.Get("/verify", auditHandler.VerifyChain)
	})

	// Backup/restore
	backupProvider := &backup.HubStateProvider{Config: cfg, WebhookLister: webhookDispatcher}
	backupHandler := backup.NewAPIHandler(backupProvider, "/data")
	r.Get("/api/backup/export", backupHandler.ExportBackup)
	r.Post("/api/backup/diff", backupHandler.DiffBackup)
	r.Post("/api/backup/import", backupHandler.ImportBackup)

	// WireGuard peer management + auto-provisioning (optional)
	if cfg.WGEnabled && cfg.WGURL != "" {
		wgClient := wireguard.NewClient(cfg.WGURL, cfg.WGPassword)
		if err := wgClient.Login(ctx); err != nil {
			slog.Warn("wireguard: login failed (peer management disabled)", "error", err)
		} else {
			wgHandler := wireguard.NewAPIHandler(wgClient)
			r.Get("/api/wireguard/peers", wgHandler.ListPeers)
			r.Post("/api/wireguard/peers", wgHandler.CreatePeer)
			r.Get("/api/wireguard/peers/{id}/config", wgHandler.GetPeerConfig)
			r.Delete("/api/wireguard/peers/{id}", wgHandler.DeletePeer)

			// Auto-provisioner: creates/deletes WG peers on device register/delete.
			wgProvisioner := wireguard.NewProvisioner(wgClient)
			wgProvisioner.Hydrate(ctx)
			deviceHandler.SetProvisioner(wgProvisioner)

			// Per-device WG config download endpoint.
			r.Get("/api/devices/{imei}/wireguard", deviceHandler.GetDeviceWireguard)

			slog.Info("wireguard: peer management + auto-provisioning enabled", "url", cfg.WGURL)
		}
	}

	// Reticulum identity, routes, relay, and topology API
	if hubIdentity != nil {
		retIdentityHandler := api.NewReticulumIdentityHandler(hubIdentity)
		r.Get("/api/reticulum/identity", retIdentityHandler.GetIdentity)
		retRoutesHandler := api.NewReticulumRoutesHandler(reticulumRouter)
		r.Get("/api/reticulum/routes", retRoutesHandler.ListRoutes)
		retRelayHandler := api.NewReticulumRelayHandler(reticulumRelay)
		r.Get("/api/reticulum/relay", retRelayHandler.GetStatus)
		retTopologyHandler := api.NewReticulumTopologyHandler(
			hubIdentity, reticulumRouter, reticulumRelay,
			reticulumPathHandler, reticulumHintPublisher,
		)
		r.Get("/api/reticulum/topology", retTopologyHandler.GetTopology)
	}

	// Tor .onion address discovery
	torHostPath := os.Getenv("HUB_TOR_HOSTNAME_PATH")
	if torHostPath == "" {
		torHostPath = "/var/lib/tor/hidden_service/hostname"
	}
	torService := hubtor.NewService(torHostPath)
	torHandler := hubtor.NewAPIHandler(torService)
	r.Get("/api/tor/onion", torHandler.GetOnion)

	// Geofence engine + API
	geoEngine := geo.NewEngine()
	geoHandler := api.NewGeofenceHandler(geoEngine)
	r.Get("/api/geofences", geoHandler.ListFences)
	r.Post("/api/geofences", geoHandler.CreateFence)
	r.Delete("/api/geofences/{id}", geoHandler.DeleteFence)

	// hawkBit OTA management (optional)
	if cfg.HawkBitEnabled && cfg.HawkBitURL != "" {
		hbClient := hawkbit.NewClient(cfg.HawkBitURL, cfg.HawkBitUsername, cfg.HawkBitPassword)
		if hbClient.IsReachable(ctx) {
			hbHandler := hawkbit.NewAPIHandler(hbClient)
			r.Get("/api/ota/targets", hbHandler.ListTargets)
			r.Post("/api/ota/targets", hbHandler.CreateTarget)
			r.Get("/api/ota/targets/{controllerId}", hbHandler.GetTarget)
			r.Delete("/api/ota/targets/{controllerId}", hbHandler.DeleteTarget)
			r.Get("/api/ota/targets/{controllerId}/actions", hbHandler.GetTargetActions)
			r.Delete("/api/ota/targets/{controllerId}/actions/{actionId}", hbHandler.CancelAction)
			r.Post("/api/ota/rollouts", hbHandler.CreateRollout)
			r.Get("/api/ota/rollouts/{id}", hbHandler.GetRollout)
			r.Post("/api/ota/rollouts/{id}/start", hbHandler.StartRollout)
			r.Post("/api/ota/rollouts/{id}/pause", hbHandler.PauseRollout)
			checker.AddProbe("hawkbit", func(ctx context.Context) error {
				if !hbClient.IsReachable(ctx) {
					return fmt.Errorf("hawkbit not reachable")
				}
				return nil
			})
			slog.Info("hawkbit: OTA management enabled", "url", cfg.HawkBitURL)
		} else {
			slog.Warn("hawkbit: server not reachable (OTA management disabled)", "url", cfg.HawkBitURL)
		}
	}

	// Message routing engine (configurable source→destination rules)
	routeEngine := routing.NewEngine(dataStore, msgBus, store.DefaultTenantID)
	// Register SMS destination handler if SMS is enabled.
	// Use the same API-key-authenticated client as the send endpoint. [MESHSAT-448]
	if smsClientForSend != nil {
		routeEngine.RegisterHandler("sms", routing.NewSMSHandler(smsClientForSend))
	}
	// Register Email destination handler if email is enabled.
	if emailKeyRing != nil {
		routeEmailClient := hubemail.NewClient(cfg.EmailSMTPHost, cfg.EmailFrom, cfg.EmailUsername, cfg.EmailPassword, emailKeyRing)
		routeEngine.RegisterHandler("email", routing.NewEmailHandler(routeEmailClient))
	}
	// Register webhook, notification, MQTT, TAK, and APRS destination handlers.
	routeEngine.RegisterHandler("webhook", routing.NewWebhookHandler(webhookDispatcher))
	routeEngine.RegisterHandler("mqtt", routing.NewMQTTHandler(msgBus))
	routeEngine.RegisterHandler("tak", routing.NewTAKHandler(msgBus))
	routeEngine.RegisterHandler("aprs", routing.NewAPRSHandler(msgBus))
	if len(notifiers) > 0 {
		routeEngine.RegisterHandler("notification", routing.NewNotificationHandler(notifiers[0]))
	}
	if msgBus.IsConnected() {
		_ = routing.SeedDefaults(ctx, dataStore, store.DefaultTenantID)
		if err := routeEngine.Start(); err != nil {
			slog.Error("routing: failed to start engine", "error", err)
		}
	}
	routeAPIHandler := routing.NewAPIHandler(dataStore, routeEngine)
	r.Get("/api/routes", routeAPIHandler.ListRoutes)
	r.Post("/api/routes/test", routeAPIHandler.TestRoutes)
	r.Post("/api/routes", routeAPIHandler.CreateRoute)
	r.Get("/api/routes/{id}", routeAPIHandler.GetRoute)
	r.Put("/api/routes/{id}", routeAPIHandler.UpdateRoute)
	r.Delete("/api/routes/{id}", routeAPIHandler.DeleteRoute)

	// IPoUGRS tunnel (experimental — IP-over-satellite)
	ipougrsConfig := ipougrs.DefaultConfig()
	ipougrsTunnel := ipougrs.NewTunnel(ipougrsConfig)
	ipougrsHandler := ipougrs.NewAPIHandler(ipougrsTunnel)
	r.Get("/api/ipougrs/status", ipougrsHandler.GetStatus)

	// Message templates (MESHSAT-312)
	templateHandler := api.NewMessageTemplateHandler(dataStore)
	r.Get("/api/message-templates", templateHandler.ListTemplates)
	r.Post("/api/message-templates", templateHandler.CreateTemplate)
	r.Get("/api/message-templates/{id}", templateHandler.GetTemplate)
	r.Put("/api/message-templates/{id}", templateHandler.UpdateTemplate)
	r.Delete("/api/message-templates/{id}", templateHandler.DeleteTemplate)
	r.Post("/api/message-templates/{id}/render", templateHandler.RenderTemplate)

	// Sensor payload codec registry
	codecRegistry := codec.NewRegistry()
	codecAPIHandler := codec.NewAPIHandler(codecRegistry)
	r.Get("/api/codecs", codecAPIHandler.ListCodecs)

	// OpenAPI spec (generated by swag)
	r.Get("/api/docs/swagger.json", func(w http.ResponseWriter, r *http.Request) {
		data, err := swaggerDocs.SpecFS.ReadFile("swagger.json")
		if err != nil {
			http.Error(w, `{"error":"swagger.json not available"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})
	r.Get("/api/docs/swagger.yaml", func(w http.ResponseWriter, r *http.Request) {
		data, err := swaggerDocs.SpecFS.ReadFile("swagger.yaml")
		if err != nil {
			http.Error(w, "swagger.yaml not available", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/x-yaml")
		_, _ = w.Write(data)
	})

	// Swagger UI — lightweight HTML page loading Swagger UI from CDN
	r.Get("/api/docs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(swaggerUIHTML))
	})
	r.Get("/api/docs/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(swaggerUIHTML))
	})

	// API versioning: /api/v1/* forwards to /api/* for backwards-compatible versioning
	r.Mount("/api/v1", http.StripPrefix("/api/v1", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.URL.Path = "/api" + req.URL.Path
		if req.URL.RawPath != "" {
			req.URL.RawPath = "/api" + req.URL.RawPath
		}
		r.ServeHTTP(w, req)
	})))

	// Embedded Vue SPA — serve from Go binary (catch-all after API routes)
	distFS, err := fs.Sub(web.DistFS, "dist")
	if err != nil {
		slog.Error("embedded SPA not available", "error", err)
	} else {
		fileServer := http.FileServer(http.FS(distFS))
		r.Handle("/*", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			// Try to serve the file; if not found, serve index.html (SPA routing)
			f, err := distFS.Open(req.URL.Path[1:]) // strip leading /
			if err != nil {
				// Serve index.html for SPA client-side routing
				req.URL.Path = "/"
			} else {
				_ = f.Close()
			}
			fileServer.ServeHTTP(w, req)
		}))
	}

	// Scheduled message delivery (MESHSAT-314).
	msgScheduler := scheduler.New(dataStore, &scheduledSenderAdapter{rock7: rock7Client}, 30*time.Second)
	go msgScheduler.Run(ctx)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		slog.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
	close(touchCh) // drain remaining API key last_used updates
	if aprsisClient != nil {
		aprsisClient.Disconnect()
	}
	if otsPoller != nil {
		otsPoller.Stop()
	}
	if takClient != nil {
		takClient.Disconnect()
	}
	msgBus.Disconnect()
	_ = otelShutdown(shutdownCtx)
	slog.Info("stopped")
}

func initLogger(cfg config.Config) {
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if cfg.LogFormat == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	slog.SetDefault(slog.New(handler))
}

// scheduledSenderAdapter adapts the Rock7 MT client to scheduler.MessageSender.
type scheduledSenderAdapter struct {
	rock7 *rock7.Client
}

func (a *scheduledSenderAdapter) SendScheduled(ctx context.Context, msg *store.Message) error {
	if a.rock7 == nil {
		return fmt.Errorf("MT send not configured")
	}
	dataHex := msg.RawHex
	if dataHex == "" {
		dataHex = hex.EncodeToString([]byte(msg.Text))
	}
	_, err := a.rock7.SendMT(ctx, msg.DeviceIMEI, dataHex)
	return err
}

// bootstrapCredentialMasterKey loads or generates the master key for credential encryption.
func bootstrapCredentialMasterKey(s store.Store) []byte {
	ctx := context.Background()
	keyHex, err := s.GetSystemConfig(ctx, "credential_master_key")
	if err == nil && keyHex != "" {
		key, err := hex.DecodeString(keyHex)
		if err == nil && len(key) == 32 {
			slog.Info("credential master key loaded from DB")
			return key
		}
	}
	// Bootstrap new key
	key, err := hubcrypto.GenerateKey()
	if err != nil {
		slog.Error("failed to generate credential master key", "error", err)
		return make([]byte, 32) // fallback — all zeros (not secure, but won't crash)
	}
	if err := s.SetSystemConfig(ctx, "credential_master_key", hex.EncodeToString(key)); err != nil {
		slog.Error("failed to persist credential master key", "error", err)
	} else {
		slog.Info("credential master key bootstrapped")
	}
	return key
}

const swaggerUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>MeshSat Hub API</title>
<link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>
SwaggerUIBundle({url:"/api/docs/swagger.json",dom_id:"#swagger-ui",presets:[SwaggerUIBundle.presets.apis,SwaggerUIBundle.SwaggerUIStandalonePreset],layout:"BaseLayout"})
</script>
</body>
</html>`
