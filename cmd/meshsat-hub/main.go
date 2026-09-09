package main

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/meshsat/meshsat-hub/internal/integrations"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/oob"
	"github.com/meshsat/meshsat-hub/internal/oob/bearers"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
	"github.com/meshsat/meshsat-hub/internal/webhookroute"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/meshsat/meshsat-hub/cmd/meshsat-hub/web"
	"github.com/meshsat/meshsat-hub/internal/alerting"
	"github.com/meshsat/meshsat-hub/internal/api"
	"github.com/meshsat/meshsat-hub/internal/apprise"
	"github.com/meshsat/meshsat-hub/internal/aprsis"
	"github.com/meshsat/meshsat-hub/internal/audit"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/authentik"
	"github.com/meshsat/meshsat-hub/internal/backup"
	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/bus/paho"
	"github.com/meshsat/meshsat-hub/internal/cloudloop"
	"github.com/meshsat/meshsat-hub/internal/codec"
	"github.com/meshsat/meshsat-hub/internal/config"
	"github.com/meshsat/meshsat-hub/internal/constellation"
	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
	"github.com/meshsat/meshsat-hub/internal/deadman"
	"github.com/meshsat/meshsat-hub/internal/dedup"
	"github.com/meshsat/meshsat-hub/internal/directory"
	hubemail "github.com/meshsat/meshsat-hub/internal/email"
	"github.com/meshsat/meshsat-hub/internal/escalation"
	"github.com/meshsat/meshsat-hub/internal/fragment"
	"github.com/meshsat/meshsat-hub/internal/geo"
	"github.com/meshsat/meshsat-hub/internal/globalstar"
	"github.com/meshsat/meshsat-hub/internal/hawkbit"
	"github.com/meshsat/meshsat-hub/internal/health"
	"github.com/meshsat/meshsat-hub/internal/ipougrs"
	"github.com/meshsat/meshsat-hub/internal/kofi"
	"github.com/meshsat/meshsat-hub/internal/leader"
	hubmessage "github.com/meshsat/meshsat-hub/internal/message"
	"github.com/meshsat/meshsat-hub/internal/metrics"
	hubmw "github.com/meshsat/meshsat-hub/internal/middleware"
	"github.com/meshsat/meshsat-hub/internal/mptcp"
	hubmsvqsc "github.com/meshsat/meshsat-hub/internal/msvqsc"
	"github.com/meshsat/meshsat-hub/internal/ntfy"
	"github.com/meshsat/meshsat-hub/internal/objstore"
	"github.com/meshsat/meshsat-hub/internal/observability"
	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/position"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/quota"
	"github.com/meshsat/meshsat-hub/internal/ratelimit"
	"github.com/meshsat/meshsat-hub/internal/reticulum"
	"github.com/meshsat/meshsat-hub/internal/rock7"
	"github.com/meshsat/meshsat-hub/internal/rockblock"
	"github.com/meshsat/meshsat-hub/internal/routing"
	"github.com/meshsat/meshsat-hub/internal/scheduler"
	"github.com/meshsat/meshsat-hub/internal/sms"
	"github.com/meshsat/meshsat-hub/internal/sos"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/dbwrap"
	"github.com/meshsat/meshsat-hub/internal/store/postgres"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/tak"
	"github.com/meshsat/meshsat-hub/internal/timesync"
	hubtor "github.com/meshsat/meshsat-hub/internal/tor"
	"github.com/meshsat/meshsat-hub/internal/webhook"
	"github.com/meshsat/meshsat-hub/internal/wireguard"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	swaggerDocs "github.com/meshsat/meshsat-hub/docs/swagger"
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
		msgBus = paho.New(brokerURL, mqttClientID(cfg.MQTTClientID))
	default: // "standalone"
		msgBus = paho.New(cfg.MQTTBrokerURL, mqttClientID(cfg.MQTTClientID))
	}
	msgBus = bus.NewObservedBus(msgBus) // Wrap with metrics instrumentation.
	if migrateOnly() {
		// --migrate-only needs the store only; do not spend the retry budget
		// on a broker the migration Job cannot reach anyway.
		slog.Info("migrate-only: skipping message bus connect")
	} else if err := msgBus.Connect(); err != nil {
		slog.Warn("bus connection failed (will retry in background)", "error", err)
	}
	checker.AddInfoProbe("mqtt", func(_ context.Context) error {
		if !msgBus.IsConnected() {
			return fmt.Errorf("mqtt not connected")
		}
		return nil
	})

	// --- Store: driver from HUB_DB_DRIVER or sniffed from the DSN ---
	dbwrap.SetDefaultMaxAttempts(cfg.DBRetryMaxAttempts)
	var dataStore store.Store
	slowQ := time.Duration(cfg.DBSlowQueryMS) * time.Millisecond
	dbDriver := cfg.ResolvedDBDriver()
	switch dbDriver {
	case "postgres":
		pgStore, err := postgres.New(cfg.DatabaseURL, slowQ)
		if err != nil {
			slog.Error("postgres connection failed", "error", err)
			os.Exit(1)
		}
		if err := pgStore.Migrate(ctx); err != nil {
			slog.Error("postgres migration failed", "error", err)
			os.Exit(1)
		}
		dataStore = pgStore
	default: // "sqlite"
		sqlStore, err := sqlite.New(cfg.SQLitePath, slowQ)
		if err != nil {
			slog.Error("sqlite open failed", "error", err, "path", cfg.SQLitePath)
			os.Exit(1)
		}
		if err := sqlStore.Migrate(ctx); err != nil {
			slog.Error("sqlite migration failed", "error", err)
			os.Exit(1)
		}
		dataStore = sqlStore
	}
	slog.Info("store ready", "driver", dbDriver, "mode", cfg.Mode)
	// --migrate-only / HUB_MIGRATE_ONLY=true: apply the schema and exit 0.
	// Used by the k8s migration rehearsal and cutover to create the Postgres
	// schema before pgloader copies the data (k8s/scripts/rehearsal/).
	if migrateOnly() {
		slog.Info("migrate-only: schema applied, exiting", "driver", dbDriver)
		_ = dataStore.Close()
		os.Exit(0)
	}
	// Tenant resolution for inbound MQTT traffic: device/bridge → owning tenant,
	// default tenant for unregistered ones (MESHSAT-864 MR 19).
	tenants := tenancy.NewResolver(dataStore, store.DefaultTenantID, 30*time.Second)

	// Readiness: the database is the one critical dependency. Stores that
	// know whether they accept writes (Galera, Postgres primary) say so.
	if prober, ok := dataStore.(store.ReadinessProber); ok {
		checker.AddProbe("db", prober.Ready)
	} else {
		checker.AddProbe("db", dataStore.Ping)
	}
	defer func() { _ = dataStore.Close() }()

	// Audit service (tamper-evident hash chain).
	auditSvc := audit.New(dataStore)
	// Audit log retention runs on the leader only (registered below once the
	// elector exists).
	auditRetentionCfg := audit.RetentionConfig{
		RetentionDays: cfg.AuditRetentionDays,
		ArchivePath:   cfg.AuditArchivePath,
	}
	if cfg.AuditArchiveS3Endpoint != "" || cfg.AuditArchiveS3Bucket != "" {
		auditRetentionCfg.S3 = &audit.S3Config{
			Endpoint: cfg.AuditArchiveS3Endpoint, Bucket: cfg.AuditArchiveS3Bucket, Prefix: cfg.AuditArchiveS3Prefix,
			Region: cfg.AuditArchiveS3Region, AccessKey: cfg.AuditArchiveS3AccessKey, SecretKey: cfg.AuditArchiveS3SecretKey,
		}
	}

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
		checker.AddInfoProbe("redis", func(ctx context.Context) error {
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
	// One identity per pod: the pid is 1 in every container, so a pid-based
	// identity made two replicas the same Lease holder (MESHSAT-711).
	instanceID := leaderInstanceID(cfg.MQTTClientID)
	switch cfg.Mode {
	case "cluster":
		leaderElector = leader.NewNATS(msgBus, instanceID)
	case "kubernetes":
		leaderElector = leader.NewKubeLease(instanceID)
	default: // "standalone"
		leaderElector = leader.NewNoop()
	}
	// Services that must run on exactly one replica (MESHSAT-910): started
	// on leadership acquisition, stopped on loss. Per-replica services (MQTT
	// subscribers, routing, in-memory caches, MPTCP monitor, HeMB reaper)
	// stay outside this set.
	leaderSingletons := leader.NewSingletons()
	if kl, ok := leaderElector.(*leader.KubeLease); ok {
		checker.AddInfoProbe("leader_election", func(_ context.Context) error { return kl.LastError() })
	}

	// Cloudloop API client for MT sends.
	// Per-tenant provider accounts (MESHSAT-977): encrypted rows in the
	// credentials table; the environment-configured values are the platform
	// account and serve the default tenant only.
	credMasterKey := bootstrapCredentialMasterKey(dataStore)
	providerAccounts := integrations.New(dataStore, credMasterKey)
	providerAccounts.SetPlatform(integrations.ProviderCloudloop, map[string]string{
		"api_url": cfg.CloudloopAPIURL, "api_key": cfg.CloudloopAPIKey, "account_id": cfg.CloudloopAccountID, "webhook_token": cfg.CloudloopWebhookToken})
	providerAccounts.SetPlatform(integrations.ProviderTwilio, map[string]string{
		"account_sid": cfg.SMSAccountSID, "auth_token": cfg.SMSAuthToken, "from_number": cfg.SMSFromNumber})
	providerAccounts.SetPlatform(integrations.ProviderRock7, map[string]string{"username": cfg.Rock7Username, "password": cfg.Rock7Password})
	providerAccounts.SetPlatform(integrations.ProviderRockBLOCK, map[string]string{"webhook_secret": cfg.RockBLOCKSecret})
	providerAccounts.SetPlatform(integrations.ProviderGlobalstar, map[string]string{
		"api_url": cfg.GlobalstarAPIURL, "api_key": cfg.GlobalstarAPIKey, "webhook_secret": cfg.GlobalstarWebhookSecret})

	cloudloopClient := cloudloop.NewClient(cfg.CloudloopAPIURL, cfg.CloudloopAPIKey)
	// One client per tenant account; the platform client serves the default tenant.
	cloudloopPool := cloudloop.NewClientPool(cloudloopClient, providerAccounts)

	// Device resolver: learns IMEI-to-thingID mappings from MO messages and Cloudloop API.
	thingResolver := cloudloop.NewThingResolver(cloudloopPool)
	if spec := os.Getenv("HUB_CLOUDLOOP_DEVICE_MAP"); spec != "" {
		slog.Info("resolver: seeded device mappings from HUB_CLOUDLOOP_DEVICE_MAP",
			"count", thingResolver.SeedFromSpec(spec))
	}
	// Tenants add Cloudloop accounts at runtime, so the refresh always runs
	// (it is a no-op while no tenant has an account).
	go thingResolver.StartPeriodicRefresh(ctx, 5*time.Minute)

	// Start MT sender (subscribes to meshsat/+/mt/send).
	mtSender := cloudloop.NewSender(cloudloopClient, msgBus)
	mtSender.SetTenants(tenants)
	mtSender.SetClientPool(cloudloopPool)
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
	if msgBus.IsConnected() { // tenants add accounts at runtime; the poller skips tenants without one
		creditPoller := cloudloop.NewTenantCreditPoller(cloudloopPool, msgBus, 1*time.Hour)
		leaderSingletons.Add("cloudloop-credit-poller", creditPoller.Start)
	}

	// Globalstar API client (optional — second satellite constellation).
	var globalstarClient *globalstar.Client
	if cfg.GlobalstarAPIKey != "" {
		globalstarClient = globalstar.NewClient(cfg.GlobalstarAPIURL, cfg.GlobalstarAPIKey)
		slog.Info("globalstar: API client enabled", "url", cfg.GlobalstarAPIURL)
	}

	// Constellation router — multi-backend satellite send.
	constellationRouter := constellation.NewRouter(constellation.StrategyAvailable)
	iridiumConst := constellation.NewIridiumBackend(cloudloopClient)
	iridiumConst.SetClientPool(cloudloopPool)
	iridiumConst.SetTenants(tenants)
	constellationRouter.Register(iridiumConst)
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
	if cfg.TAKAPIBaseURL != "" && cfg.TAKAPIUsername != "" {
		leaderSingletons.Add("tak-ots-poller", func(sctx context.Context) {
			p := tak.NewOTSPoller(cfg.TAKAPIBaseURL, cfg.TAKAPIUsername, cfg.TAKAPIPassword, cfg.TAKAPIPollSec, msgBus, dataStore, store.DefaultTenantID)
			p.SetMaxDevices(cfg.TAKAPIMaxDevices)
			p.Start()
			<-sctx.Done()
			p.Stop()
		})
	}

	var takFederation *tak.Federation
	var aprsisClient *aprsis.Client

	// The elector itself is started at the end of startup (see
	// leader.RunWith below), after every singleton has been registered.
	onLeaderAcquired := func() {
		// onAcquired: Federation + APRS-IS (connection-holding services that
		// need explicit teardown); the rest of the singleton set is started by
		// RunWith.
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
	}
	onLeaderLost := func() {
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
	}

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
		posSub = position.NewSubscriber(msgBus, dataStore, tenants)
		if err := posSub.Start(); err != nil {
			slog.Error("position: failed to start subscriber", "error", err)
		}
	}

	// Message subscriber: persists MO decoded messages from MQTT to the database.
	if msgBus.IsConnected() {
		msgSub := hubmessage.NewSubscriber(msgBus, dataStore, tenants)
		if err := msgSub.Start(); err != nil {
			slog.Error("message: failed to start subscriber", "error", err)
		}
	}

	// Bridge lifecycle subscriber: auto-provisions bridges and devices from MQTT birth/death/health.
	var bridgeCommander *bridge.Commander
	var bridgeSub *bridge.Subscriber
	var hembReassemblyBuf *protocol.HeMBReassemblyBuffer
	if msgBus.IsConnected() {
		bridgeSub = bridge.NewSubscriber(msgBus, dataStore, tenants)

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
		timeout := time.Duration(cfg.BridgeOfflineTimeout) * time.Second
		leaderSingletons.Add("bridge-reaper", func(sctx context.Context) {
			r := bridge.NewReaper(dataStore, timeout)
			r.Start()
			<-sctx.Done()
			r.Stop()
		})
	}

	// Bridge certificate authority for MQTT TLS client certs.
	var bridgeCA *bridge.CertAuthority
	// Operator-supplied paths: cleaned and required absolute (a relative or
	// traversing value is ignored rather than resolved against the cwd).
	caCertPath := filepath.Clean(os.Getenv("MESHSAT_BRIDGE_CA_CERT"))
	caKeyPath := filepath.Clean(os.Getenv("MESHSAT_BRIDGE_CA_KEY"))
	if filepath.IsAbs(caCertPath) && filepath.IsAbs(caKeyPath) {
		certPEM, err := os.ReadFile(caCertPath) // #nosec G703 -- operator configuration (env), cleaned and absolute; not request input
		if err != nil {
			slog.Error("bridge-ca: failed to read CA cert", "path", caCertPath, "error", err)
		} else {
			keyPEM, err := os.ReadFile(caKeyPath) // #nosec G703 -- operator configuration (env), cleaned and absolute; not request input
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

	// On Kubernetes the CA certificate lives in a Secret mounted by NATS and
	// stunnel; the Hub keeps it current (get/update on a pre-created Secret)
	// and reports the state as the informational probe bridge_ca_export.
	if bridgeCA != nil && cfg.BridgeCASecretName != "" {
		caWriter, err := bridge.NewCASecretWriter(cfg.BridgeCASecretName, cfg.BridgeCASecretKey)
		if err != nil {
			slog.Error("bridge-ca: secret writer unavailable", "error", err)
			checker.AddInfoProbe("bridge_ca_export", func(_ context.Context) error { return err })
		} else {
			checker.AddInfoProbe("bridge_ca_export", func(_ context.Context) error { return caWriter.LastError() })
			ca := bridgeCA
			go caWriter.Run(ctx, 10*time.Minute, func() []byte { return ca.CACertPEM() })
		}
	}

	// Per-bridge NATS users and permissions rendered into a Secret that the NATS
	// pod includes and reloads (MESHSAT-864 MR 21); probe nats_auth_export.
	var natsAuth *bridge.NATSAuthSyncer
	if cfg.NATSAuthSecretName != "" {
		w, err := bridge.NewNATSAuthSecretWriter(cfg.NATSAuthSecretName, "users.conf")
		if err != nil {
			slog.Error("nats-auth: secret writer unavailable", "error", err)
			checker.AddInfoProbe("nats_auth_export", func(_ context.Context) error { return err })
		} else {
			natsAuth = bridge.NewNATSAuthSyncer(dataStore, w, hubmqtt.Namespace, 5*time.Minute)
			checker.AddInfoProbe("nats_auth_export", func(_ context.Context) error { return natsAuth.LastError() })
			go natsAuth.Run(ctx)
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
	if exportPath := filepath.Clean(cfg.BridgeCACertExportPath); bridgeCA != nil && filepath.IsAbs(exportPath) {
		if err := os.WriteFile(exportPath, bridgeCA.CACertPEM(), 0644); err != nil { // #nosec G306 G703 -- cleaned absolute path; the payload is the public CA certificate read by the NATS container
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
		checker.AddInfoProbe("apprise", appriseClient.Healthz)
		slog.Info("apprise: notification backend enabled", "url", cfg.AppriseURL)
	}
	if cfg.NtfyEnabled && cfg.NtfyURL != "" {
		ntfyClient := ntfy.New(cfg.NtfyURL)
		if cfg.NtfyToken != "" {
			ntfyClient.SetToken(cfg.NtfyToken)
		}
		notifiers = append(notifiers, ntfyClient)
		checker.AddInfoProbe("ntfy", ntfyClient.Healthz)
		slog.Info("ntfy: notification backend enabled", "url", cfg.NtfyURL)
	}
	// Twilio: the platform account (env) serves the default tenant; every other
	// tenant brings its own on the Integrations page (MESHSAT-977).
	var smsPlatform *sms.Client
	if cfg.SMSEnabled && cfg.SMSAccountSID != "" {
		if cfg.SMSAPIKeySID != "" {
			smsPlatform = sms.NewClientWithAPIKey(cfg.SMSAccountSID, cfg.SMSAPIKeySID, cfg.SMSAuthToken, cfg.SMSFromNumber)
			slog.Info("sms: using API key auth", "key_sid", cfg.SMSAPIKeySID)
		} else {
			smsPlatform = sms.NewClient(cfg.SMSAccountSID, cfg.SMSAuthToken, cfg.SMSFromNumber)
		}
	}
	smsPool := sms.NewClientPool(smsPlatform, providerAccounts)
	if cfg.SMSEnabled {
		notifiers = append(notifiers, sms.NewNotifierPool(smsPool))
		slog.Info("sms: escalation notifier enabled", "platform_from", cfg.SMSFromNumber)
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
	leaderSingletons.Add("escalation-loop", escEngine.Start)

	// Alert rules evaluator (configurable alerting engine, MESHSAT-313).
	alertEval := alerting.New(dataStore, &escalationAdapter{engine: escEngine}, 60*time.Second)
	leaderSingletons.Add("alert-evaluator", alertEval.Start)

	// Dead man's switch monitor (triggers escalation on missed device check-ins).
	deadmanMonitor := deadman.NewMonitor(dataStore, escEngine)
	leaderSingletons.Add("deadman-monitor", deadmanMonitor.Start)
	leaderSingletons.Add("audit-retention", func(sctx context.Context) {
		audit.RunRetention(sctx, dataStore, auditRetentionCfg)
	})

	// Wire dead man's switch to position subscriber so device positions reset the timer.
	if posSub != nil {
		posSub.SetDeadman(deadmanMonitor)
	}

	// SOS detector (subscribes to mo/decoded, triggers escalation on SOS messages).
	if msgBus.IsConnected() {
		sosDetector := sos.NewDetector(msgBus, escEngine, dataStore, tenants, cfg.SOSChainID)
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
	checker.AddInfoProbe("reticulum_identity", func(_ context.Context) error {
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
		iridiumBackend.SetClientPool(cloudloopPool)
		iridiumBackend.SetTenants(tenants)
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
	rbHandler.SetTenants(tenants)
	rbHandler.SetAccounts(providerAccounts)
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
	rock7Pool := rock7.NewClientPool(rock7Client, providerAccounts)

	// Out-of-band bridge commands (MESHSAT-964 C): sealed OOB frames over the
	// kit's SIM (Twilio), a 9704 modem (Cloudloop IMT) or a 9603 modem
	// (Rock7 MT), replies classified out of the three inbound webhooks.
	oobSvc := oob.New(dataStore, credMasterKey, auditSvc, oob.Options{Encrypt: cfg.OOBEncrypt, MaxPerHour: cfg.OOBMaxPerHour})
	oobSvc.RegisterTransport(oob.BearerSMS, &bearers.SMS{Pool: smsPool, Wait: cfg.OOBSMSTimeout})
	oobSvc.RegisterTransport(oob.BearerIMT, &bearers.IMT{Pool: cloudloopPool, Resolver: thingResolver, Wait: cfg.OOBSatTimeout})
	oobSvc.RegisterTransport(oob.BearerSBD, &bearers.SBD{Pool: rock7Pool, Wait: cfg.OOBSatTimeout})
	if bridgeCommander != nil {
		bridgeCommander.SetOOB(oobSvc, func(imei string) bool {
			_, imt := thingResolver.Resolve(tenants.ForDevice(context.Background(), imei), imei)
			return imt
		})
	}
	rbHandler.SetOOB(oobSvc)

	// Globalstar MO webhook handler.
	gsHandler := globalstar.NewHandler(msgBus, cfg.GlobalstarWebhookSecret)
	gsHandler.SetTenants(tenants)
	gsHandler.SetAccounts(providerAccounts)
	gsHandler.SetAudit(auditSvc)
	gsHandler.SetDedup(dedupTracker)
	gsHandler.SetReassembler(reassembler)
	gsHandler.SetKeyStore(keyStore)
	gsHandler.SetDeadman(deadmanMonitor)
	gsHandler.SetMSVQSC(msvqscDecoder)

	// Cloudloop LingoMO webhook handler.
	clHandler := cloudloop.NewWebhookHandler(msgBus)
	clHandler.SetTenants(tenants)
	clHandler.SetAccounts(providerAccounts)
	clHandler.SetOOB(oobSvc)
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
	clHandler.SetToken(cfg.CloudloopWebhookToken)
	if strings.TrimSpace(cfg.CloudloopWebhookAllowedIPs) == "*" && cfg.CloudloopWebhookToken == "" {
		slog.Error("cloudloop: HUB_CLOUDLOOP_WEBHOOK_ALLOWED_IPS is * without HUB_CLOUDLOOP_WEBHOOK_TOKEN; the webhook will reject every request (MESHSAT-971)")
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
	// Leader-only: Cloudloop's broker is AWS IoT, which keeps one session per client id, so
	// two replicas connecting with the same id evict each other in a loop and MO messages are
	// missed while it flaps (MESHSAT-711). A fresh subscriber is built on every acquisition
	// because Stop closes the subscriber's stop channel and Start does not reopen it.
	if cfg.CloudloopAccountID != "" && cfg.CloudloopMQTTBroker != "" {
		clMQTTCfg := cloudloop.MQTTSubscriberConfig{
			BrokerURL:  cfg.CloudloopMQTTBroker,
			CACertFile: cfg.CloudloopMQTTCACert,
			CertFile:   cfg.CloudloopMQTTCert,
			KeyFile:    cfg.CloudloopMQTTKey,
			AccountID:  cfg.CloudloopAccountID,
		}
		leaderSingletons.Add("cloudloop-mqtt", func(sctx context.Context) {
			sub := cloudloop.NewMQTTSubscriber(clMQTTCfg, clHandler.ProcessLingoMO)
			if err := sub.Start(sctx); err != nil {
				slog.Error("cloudloop mqtt: failed to start subscriber", "error", err)
				return
			}
			<-sctx.Done()
			sub.Stop()
		})
	}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	// middleware.RealIP was removed (chi v5.3.0 deprecates it as spoofable —
	// GHSA-3fxj-6jh8-hvhx): it rewrote r.RemoteAddr from X-Forwarded-For /
	// True-Client-IP before the webhook IP allowlists ran, defeating their
	// "direct peer only" checks (internal/cloudloop/webhook.go isAllowedIP
	// implements trusted-proxy XFF handling itself). RemoteAddr is now always
	// the direct TCP peer. Consumers needing the end-client IP behind the
	// reverse proxy should use chi's ClientIPFromXFFTrustedProxies + GetClientIP.
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
	if authMode == "local" || authMode == "oidc" {
		if len(cfg.JWTSigningKey) < 32 {
			slog.Error("auth: HUB_JWT_SIGNING_KEY must be at least 32 characters for local and oidc auth modes")
			os.Exit(1)
		}
		jwtSecret = []byte(cfg.JWTSigningKey)
	}
	// Local email/password login: always in local mode; in oidc mode only as
	// the HUB_LOCAL_LOGIN_ENABLED=true break-glass (authentik outage).
	localLogin := authMode == "local" || (authMode == "oidc" && cfg.LocalLoginEnabled != nil && *cfg.LocalLoginEnabled)
	authCfg := hubauth.Config{
		Mode:              authMode,
		Token:             cfg.AuthToken,
		OIDCIssuerURL:     cfg.OIDCIssuerURL,
		OIDCAudience:      cfg.OIDCAudience,
		JWTSecret:         jwtSecret,
		OIDCCertPin:       cfg.OIDCCertPin,
		OIDCCertPinBackup: cfg.OIDCCertPinBackup,
	}
	var (
		oidcClient   *hubauth.OIDCClient
		oidcHandler  *api.OIDCHandler
		loginHandler *api.LoginHandler
		sessionMgr   *hubauth.SessionManager
	)
	if authMode == "oidc" {
		authCfg.Provider = hubauth.NewJWKSProvider(cfg.OIDCIssuerURL, hubauth.OIDCHTTPClient(authCfg))
		sessionMgr = hubauth.NewSessionManager(jwtSecret, "meshsat-hub")
		loginHandler = api.NewLoginHandler(dataStore, sessionMgr, auditSvc)
		if cfg.OIDCClientID != "" && cfg.OIDCClientSecret != "" && cfg.OIDCRedirectURI != "" {
			oidcClient = &hubauth.OIDCClient{
				Provider:     authCfg.Provider,
				ClientID:     cfg.OIDCClientID,
				ClientSecret: cfg.OIDCClientSecret,
				RedirectURI:  cfg.OIDCRedirectURI,
				Scopes:       cfg.OIDCScopes,
				HTTPClient:   hubauth.OIDCHTTPClient(authCfg),
			}
		} else {
			slog.Warn("auth: oidc mode without HUB_OIDC_CLIENT_ID/SECRET/REDIRECT_URI — browser login disabled, bearer tokens only")
		}
		modes := []string{}
		if oidcClient != nil {
			modes = append(modes, "oidc")
		}
		if localLogin {
			modes = append(modes, "local")
		}
		oidcHandler = api.NewOIDCHandler(dataStore, loginHandler, oidcClient, api.OIDCConfig{
			GroupsClaim:         cfg.OIDCGroupsClaim,
			AdminGroup:          cfg.OIDCAdminGroup,
			BootstrapOwnerEmail: cfg.OIDCBootstrapOwnerEmail,
			StateKey:            jwtSecret,
			TenantEnforce:       cfg.TenantEnforce,
			SignupURL:           cfg.OIDCSignupURL,
			CommunityURL:        cfg.CommunityURL,
		}, modes)
		authCfg.Resolver = oidcHandler
	}
	r.Use(hubauth.Middleware(authCfg))
	// Tenant isolation middleware — resolves tenant from JWT claim / X-Tenant-ID header / default.
	// Enforce mode disabled for backward compatibility; enable via HUB_TENANT_ENFORCE=true.
	// A suspended or deleted tenant is refused every request. Cached for a
	// few seconds so this is not a database round trip per call.
	tenantStatus := tenancy.NewStatusCache(dataStore, 15*time.Second).WithBus(msgBus)
	if err := tenantStatus.Subscribe(); err != nil {
		slog.Warn("tenant status invalidation not subscribed; a change applies elsewhere within the cache TTL", "error", err)
	}
	hubauth.SetTenantStatusLookup(tenantStatus.Status)

	// Subscription tiers (MESHSAT-989). The ceiling is on REGISTERING devices
	// and bridges and on nothing else: ingest, delivery, the dead man's switch
	// and SOS are never gated by it, so a lapsed or over-cap tenant still gets
	// its emergency traffic through. Limits are overridable from config so a
	// tier can be re-priced without a deploy.
	for plan, limit := range cfg.PlanDeviceLimits {
		if plans.SetLimit(plan, limit) {
			slog.Info("plan limit overridden from config", "plan", plan, "devices", limit)
		} else {
			slog.Warn("unknown plan in plan_device_limits, ignored", "plan", plan, "known", plans.Names())
		}
	}
	// The plan comes from the same cached tenant read the status middleware
	// already does, so a create costs two counts and no extra tenant query.
	quotaChecker := quota.New(dataStore, tenantStatus.Plan)
	if bridgeSub != nil {
		bridgeSub.SetQuota(quotaChecker)
	}
	// Destroying a closed tenant's data is single-owner work and its audit
	// line should be written once, so it runs on the lease holder.
	leaderSingletons.Add("tenant-purge", tenancy.NewPurgeJob(dataStore, auditSvc, 0).Run)
	r.Use(hubauth.TenantMiddleware(cfg.TenantEnforce))
	r.Use(metrics.ChiMiddleware)
	r.Use(hubmw.Logging) // Structured HTTP request logging (runs last to see auth context).

	r.Get("/healthz", health.LivezHandler)
	r.Get("/readyz", checker.ReadyzHandler)
	r.Get("/startupz", checker.StartupzHandler)
	r.Handle("/metrics", api.MetricsTokenGuard(cfg.MetricsToken, metrics.Handler()))

	// Self-hosted vector basemap (MESHSAT-967). Public OpenStreetMap-derived
	// data streamed out of the object store with range requests, so the map in
	// the browser asks no third-party tile host for anything.
	if h := basemapHandler(cfg); h != nil {
		r.Get("/basemap/basemap.pmtiles", h.ServeHTTP)
		r.Head("/basemap/basemap.pmtiles", h.ServeHTTP)
		r.Get("/basemap/assets/*", h.ServeAsset)
		if h.HasLocal() {
			r.Get("/basemap/local.pmtiles", h.ServeLocal)
			r.Head("/basemap/local.pmtiles", h.ServeLocal)
		}
		slog.Info("basemap: serving /basemap/", "object", h.Describe(), "local", h.HasLocal())
	}

	// pprof profiling endpoints (opt-in, behind auth).
	if cfg.PprofEnabled {
		slog.Warn("pprof endpoints enabled at /debug/pprof/ — ensure auth is configured")
		r.HandleFunc("/debug/pprof/", pprof.Index)
		r.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		r.HandleFunc("/debug/pprof/profile", pprof.Profile)
		r.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		r.HandleFunc("/debug/pprof/trace", pprof.Trace)
		r.HandleFunc("/debug/pprof/{profile}", func(w http.ResponseWriter, req *http.Request) {
			pprof.Handler(chi.URLParam(req, "profile")).ServeHTTP(w, req)
		})
	}

	// WebSocket real-time event hub
	wsHub := api.NewWSHub()
	r.Get("/api/ws", wsHub.HandleWS)
	// Bridge MQTT events to WebSocket for live dashboard updates.
	//
	// Two things here are load-bearing. The filters go through DualFilters, or
	// only the default tenant's traffic is ever seen -- meshsat/+/position does
	// not match meshsat/{tenant}/{device}/position, so every other tenant's
	// dashboard sat silent. And delivery is per tenant, resolved from the topic
	// itself: the hub used to write every frame to every connected client
	// regardless of tenant, which put one tenant's positions, messages and SOS
	// events on every other tenant's socket.
	if msgBus.IsConnected() {
		for _, legacy := range []string{"meshsat/+/mo/decoded", "meshsat/+/position", "meshsat/+/sos"} {
			for _, filter := range hubmqtt.DualFilters(legacy) {
				_ = msgBus.Subscribe(filter, 0, func(topic string, payload []byte) {
					tenantID, _, _, ok := hubmqtt.ParseDeviceTopic(topic)
					if !ok {
						slog.Debug("ws: unparseable device topic, not delivered", "topic", topic)
						return
					}
					wsHub.BroadcastTenant(tenantID, payload)
				})
			}
		}
	}
	// Webhook endpoints — rate limited to 60 requests/minute per source IP.
	//
	// Each tenant configures its provider console with its own path, whose last
	// segment is that tenant's secret (MESHSAT-975). The middleware turns that
	// secret into exactly one tenant before the handler runs, so a handler
	// never has to work out whose message this is and can never guess wrong.
	// An unknown secret is a 404.
	//
	// The unsuffixed paths below are the platform tenant's, kept for one
	// release while the consoles are re-pointed. They no longer resolve a
	// tenant from the payload.
	webhookRoute := func(provider, fieldKey, path string, h http.HandlerFunc) {
		resolve := webhookroute.Middleware(providerAccounts, provider, fieldKey)
		r.Post(path+"/{"+webhookroute.URLParam+"}",
			hubmw.WebhookRateLimit(resolve(h), 60).ServeHTTP)
		r.Post(path, hubmw.WebhookRateLimit(h, 60).ServeHTTP)
	}
	webhookRoute(integrations.ProviderRockBLOCK, "webhook_secret", "/api/webhook/rockblock", rbHandler.ServeHTTP)
	webhookRoute(integrations.ProviderGlobalstar, "webhook_secret", "/api/webhook/globalstar", gsHandler.ServeHTTP)
	webhookRoute(integrations.ProviderCloudloop, "webhook_token", "/api/webhook/cloudloop", clHandler.ServeHTTP)

	// Ko-fi subscription webhook (MESHSAT-989). Platform-level rather than
	// per-tenant: there is one Ko-fi account, and the payment says which
	// tenant it is for through a claim code in its message. The path secret
	// keeps the endpoint off scanners and out of logs; Ko-fi's
	// verification_token in the body is what authenticates it.
	if cfg.KofiWebhookSecret != "" && cfg.KofiVerificationToken != "" {
		kofiHandler := kofi.NewHandler(dataStore, cfg.KofiVerificationToken)
		kofiHandler.SetAudit(auditSvc)
		kofiHandler.SetInvalidator(tenantStatus.Forget)
		if len(cfg.KofiTierMap) > 0 {
			kofiHandler.SetTierMapping(cfg.KofiTierMap)
		}
		want := cfg.KofiWebhookSecret
		r.Post("/api/webhook/kofi/{secret}", hubmw.WebhookRateLimit(http.HandlerFunc(
			func(w http.ResponseWriter, req *http.Request) {
				// A wrong secret is a 404, not a 401: the endpoint should not
				// confirm it exists to somebody guessing at it.
				if subtle.ConstantTimeCompare([]byte(chi.URLParam(req, "secret")), []byte(want)) != 1 {
					http.NotFound(w, req)
					return
				}
				kofiHandler.ServeHTTP(w, req)
			}), 60).ServeHTTP)
		// Downgrading a lapsed plan is single-owner work and its audit line
		// should be written once, so it runs on the lease holder.
		leaderSingletons.Add("subscription-lapse",
			kofi.NewLapseJob(dataStore, auditSvc, tenantStatus.Forget).Run)
		slog.Info("kofi: subscription webhook enabled")
	} else {
		slog.Info("kofi: subscription webhook disabled; set HUB_KOFI_WEBHOOK_SECRET and HUB_KOFI_VERIFICATION_TOKEN to enable")
	}

	// QR provision claim — unauthenticated (nonce IS the auth, single-use, 30min TTL).
	provisionClaimHandler := api.NewBridgeProvisionHandler(dataStore, bridgeCA, directoryTrustAnchor)
	provisionClaimHandler.SetNATSAuth(natsAuth)
	r.Get("/api/bridges/{id}/provision/{nonce}", provisionClaimHandler.ClaimProvision)

	// SMS gateway (optional — inbound webhook + outbound subscriber + send API)
	if cfg.SMSEnabled {
		smsWebhook := sms.NewWebhookHandler(msgBus, cfg.SMSWebhookSecret)
		smsWebhook.SetTenants(tenants)
		smsWebhook.SetAccounts(providerAccounts)
		smsWebhook.SetOOB(oobSvc)
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
		webhookRoute(integrations.ProviderTwilio, "webhook_token", "/api/webhook/sms", smsWebhook.ServeHTTP)
		if msgBus.IsConnected() {
			smsSub := sms.NewSubscriber(smsPlatform, msgBus)
			smsSub.SetClientPool(smsPool)
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
		smsInSub.SetTenants(tenants)
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
		emailWebhook.SetSecret(cfg.EmailWebhookSecret)
		if cfg.EmailWebhookSecret == "" {
			slog.Warn("email: HUB_EMAIL_WEBHOOK_SECRET unset; /api/webhook/email rejects every request (MESHSAT-976)")
		}
		webhookRoute(integrations.ProviderEmail, "webhook_secret", "/api/webhook/email", emailWebhook.ServeHTTP)

		emailAPIHandler := hubemail.NewAPIHandler(emailKeyRing)
		r.Get("/api/email/keys/public", emailAPIHandler.GetPublicKey)
		r.Get("/api/email/keys", emailAPIHandler.ListContacts)
		r.Post("/api/email/keys", emailAPIHandler.AddContact)
		r.Delete("/api/email/keys/{email}", emailAPIHandler.DeleteContact)
		// Owner-only: TestSend takes a caller-supplied recipient, subject and
		// body and sends them through the Hub's own SMTP identity. Without a
		// role check any viewer of any tenant could relay mail under this
		// domain's sending reputation.
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Post("/api/email/test", emailAPIHandler.TestSend)
		emailAPIHandler.SetClient(hubemail.NewClient(cfg.EmailSMTPHost, cfg.EmailFrom, cfg.EmailUsername, cfg.EmailPassword, emailKeyRing))
	}

	// Auth info
	r.Get("/api/auth/me", api.AuthMeHandler)

	// Login method discovery for the SPA (auth-exempt).
	switch {
	case oidcHandler != nil:
		r.Get("/api/auth/config", oidcHandler.Config)
		r.Get("/api/auth/oidc/login", oidcHandler.Login)
		r.Get("/api/auth/oidc/callback", oidcHandler.Callback)
	case authMode == "local":
		r.Get("/api/auth/config", api.AuthConfigHandler([]string{"local"}, cfg.CommunityURL))
	default:
		r.Get("/api/auth/config", api.AuthConfigHandler([]string{authMode}, cfg.CommunityURL))
	}

	// Session endpoints (login/refresh/logout — exempt from auth middleware).
	// Local mode: all three. OIDC mode: refresh/logout always (the OIDC
	// callback issues the same session), password login only as break-glass.
	if authMode == "local" || authMode == "oidc" {
		if loginHandler == nil {
			sessionMgr = hubauth.NewSessionManager(jwtSecret, "meshsat-hub")
			loginHandler = api.NewLoginHandler(dataStore, sessionMgr, auditSvc)
		}
		if localLogin {
			r.Post("/api/auth/login", loginHandler.Login)
			if authMode == "oidc" {
				slog.Warn("auth: HUB_LOCAL_LOGIN_ENABLED=true — password login is open alongside OIDC (break-glass)")
			}
		}
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

	// Tenant self-service (members read, owners manage invites) and the
	// platform-admin tenant directory (MESHSAT-916, MR 16).
	tenantHandler := api.NewTenantHandler(dataStore)
	tenantHandler.SetStatusInvalidator(tenantStatus.Forget)
	usageHandler := api.NewTenantUsageHandler(quotaChecker, dataStore)
	api.SetUpgradeURL(cfg.UpgradeURL)
	offboarding := api.NewTenantOffboardingHandler(dataStore, auditSvc, tenantStatus.Forget)
	r.Route("/api/tenant", func(r chi.Router) {
		r.With(hubauth.RequireRole(hubauth.RoleViewer)).Get("/", tenantHandler.Get)
		r.With(hubauth.RequireRole(hubauth.RoleViewer)).Get("/usage", usageHandler.Usage)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Put("/", tenantHandler.Update)
		// Take your data with you, or have it destroyed. Owner only.
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Get("/export", offboarding.Export)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Delete("/", offboarding.Delete)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Get("/invites", tenantHandler.ListInvites)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Post("/invites", tenantHandler.CreateInvite)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Delete("/invites/{id}", tenantHandler.DeleteInvite)
		// Provider accounts (MESHSAT-977): owners manage, members see the masked state.
		intH := api.NewTenantIntegrationsHandler(providerAccounts, auditSvc)
		r.With(hubauth.RequireRole(hubauth.RoleViewer)).Get("/integrations", intH.List)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Put("/integrations/{provider}", intH.Put)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Delete("/integrations/{provider}", intH.Delete)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Post("/integrations/{provider}/test", intH.Test)
	})
	// Approving a beta request without leaving the Hub (MESHSAT-978). Without
	// an authentik token the endpoints say so and the script stays the way.
	akClient := authentik.New(cfg.AuthentikURL, cfg.AuthentikToken)
	if akClient == nil {
		slog.Info("signups: HUB_AUTHENTIK_TOKEN unset; approve with k8s/scripts/authentik/run-bootstrap.sh")
	}
	signupHandler := api.NewSignupHandler(akClient, auditSvc, cfg.SignupWebhookURL)
	r.Route("/api/admin/signups", func(r chi.Router) {
		r.Use(hubauth.RequirePlatformAdmin())
		r.Get("/", signupHandler.List)
		r.Post("/{id}/approve", signupHandler.Approve)
		r.Post("/{id}/reject", signupHandler.Reject)
	})
	r.Route("/api/admin/tenants", func(r chi.Router) {
		r.Use(hubauth.RequirePlatformAdmin())
		r.Get("/", tenantHandler.AdminList)
		r.Put("/{id}", tenantHandler.AdminUpdate)
		r.Delete("/{id}", offboarding.AdminDelete)
		r.Get("/{id}/usage", usageHandler.AdminUsage)
	})

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
	credHandler := api.NewCredentialHandler(dataStore, credMasterKey) // key bootstrapped with providerAccounts above
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
	bridgeHandler.SetNATSAuth(natsAuth)
	bridgeHandler.SetQuota(quotaChecker)
	r.Get("/api/bridges", bridgeHandler.ListBridges)
	r.Post("/api/bridges", bridgeHandler.CreateBridge)
	r.Get("/api/bridges/{id}", bridgeHandler.GetBridge)
	r.Put("/api/bridges/{id}", bridgeHandler.UpdateBridge)
	r.Delete("/api/bridges/{id}", bridgeHandler.DeleteBridge)
	if bridgeCommander != nil {
		bridgeCmdHandler := api.NewBridgeCommandHandler(dataStore, bridgeCommander)
		r.Post("/api/bridges/{id}/command", bridgeCmdHandler.SendCommand)
	}
	// Out-of-band pairing (MESHSAT-964 C); owners only.
	oobAPI := api.NewBridgeOOBHandler(dataStore, oobSvc, bridgeCommander)
	r.Get("/api/bridges/{id}/oob", oobAPI.Get)
	r.With(hubauth.RequireRole(hubauth.RoleOwner)).Post("/api/bridges/{id}/oob", oobAPI.Pair)
	r.With(hubauth.RequireRole(hubauth.RoleOwner)).Post("/api/bridges/{id}/oob/provision", oobAPI.Provision)
	r.With(hubauth.RequireRole(hubauth.RoleOwner)).Delete("/api/bridges/{id}/oob", oobAPI.Unpair)

	// Bridge MQTT authentication API
	bridgeAuthHandler := api.NewBridgeAuthHandler(dataStore, bridgeCA)
	bridgeAuthHandler.SetNATSAuth(natsAuth)
	r.Post("/api/bridges/{id}/credentials", bridgeAuthHandler.GenerateCredentials)
	r.Post("/api/bridges/{id}/certificate", bridgeAuthHandler.IssueCertificate)
	r.Post("/api/bridges/acl/regenerate", bridgeAuthHandler.RegenerateACL)

	// One-step bridge provisioning with QR code (MESHSAT-414)
	provisionHandler := api.NewBridgeProvisionHandler(dataStore, bridgeCA, directoryTrustAnchor)
	provisionHandler.SetNATSAuth(natsAuth)
	r.Post("/api/bridges/{id}/provision", provisionHandler.Provision)
	r.Post("/api/bridges/{id}/provision/qr", provisionHandler.ProvisionQR)

	// Directory REST — tenant-scoped contacts + signed snapshot
	// [MESHSAT-538]. Opens a dedicated *sql.DB connection on the
	// same SQLite file so the directory package can own its
	// transactions without threading through the dbwrap interface
	// (which doesn't expose BeginTx). Only wired in standalone mode
	// today; cluster/MariaDB support lands in a follow-up once a
	// MariaDB-compatible SQLStore is added.
	if dbDriver == "sqlite" {
		directoryRawDB, err := sql.Open("sqlite", "file:"+cfg.SQLitePath+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=ON&_synchronous=NORMAL")
		if err != nil {
			slog.Error("directory: open raw sqlite failed", "error", err)
		} else {
			directoryRawDB.SetMaxOpenConns(1)
			directorySQLStore := directory.NewSQLStore(directoryRawDB)
			if err := directorySQLStore.Migrate(ctx); err != nil {
				slog.Error("directory: migrate failed", "error", err)
			} else {
				// Seed STANAG 4406 precedence → strategy fall-backs for
				// the default tenant. Idempotent; future per-tenant
				// lifecycle code should call this on tenant creation.
				// [MESHSAT-547 / S2-04]
				if n, err := directorySQLStore.SeedPrecedenceDefaults(ctx, store.DefaultTenantID); err != nil {
					slog.Warn("directory: precedence defaults seed failed", "error", err)
				} else if n > 0 {
					slog.Info("directory: seeded precedence defaults", "tenant", store.DefaultTenantID, "inserted", n)
				}
				directoryHandler := api.NewDirectoryHandler(directorySQLStore, directoryTrustAnchor)
				r.Get("/api/v1/directory/contacts", directoryHandler.ListContacts)
				r.Post("/api/v1/directory/contacts", directoryHandler.CreateContact)
				r.Get("/api/v1/directory/contacts/{id}", directoryHandler.GetContact)
				r.Put("/api/v1/directory/contacts/{id}", directoryHandler.UpdateContact)
				r.Delete("/api/v1/directory/contacts/{id}", directoryHandler.DeleteContact)
				// NOTE: group + policy CRUD handlers will land in a
				// follow-up story. The Store layer already supports
				// them; the REST surface is deferred until a consuming
				// UI exists. Bridge-side PutPolicy + PutGroup are
				// exercised via directory_push snapshot import — the
				// directory table of record can still be populated via
				// the snapshot endpoint below.
				r.Get("/api/v1/directory/snapshot", directoryHandler.GetSnapshot)
				// vCard 4.0 / CSV import + export [MESHSAT-541]
				r.Post("/api/v1/directory/import/vcard", directoryHandler.ImportVCard)
				r.Post("/api/v1/directory/import/csv", directoryHandler.ImportCSV)
				r.Get("/api/v1/directory/export/vcard", directoryHandler.ExportVCard)
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
	deviceHandler.SetQuota(quotaChecker)
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
	sendHandler.SetSMSClient(smsPlatform)
	sendHandler.SetSMSPool(smsPool)
	sendHandler.SetRock7Pool(rock7Pool)
	sendHandler.SetIMTSender(mtSender)
	r.Post("/api/devices/{imei}/send", sendHandler.SendMessage)
	// Explicit provider routes fail loudly on protocol mismatch [MESHSAT-750].
	// chi gives static segments precedence over {imei}, so these coexist.
	r.Post("/api/devices/rock7/{imei}/send", sendHandler.SendMessageRock7)
	r.Post("/api/devices/cloudloop/{imei}/send", sendHandler.SendMessageCloudloop)
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
	// Owner-only: an override exempts a device from the send budget entirely,
	// and the send path costs real satellite airtime. Reading usage stays open
	// to any member; disabling the limiter does not.
	r.With(hubauth.RequireRole(hubauth.RoleOwner)).Post("/api/ratelimit/{deviceID}/override", rateLimitHandler.PostOverride)
	r.With(hubauth.RequireRole(hubauth.RoleOwner)).Delete("/api/ratelimit/{deviceID}/override", rateLimitHandler.DeleteOverride)
	r.Get("/api/webhooks", webhookAPIHandler.ListWebhooks)
	// Owner-only: registering an outbound webhook makes the Hub fetch a
	// caller-supplied URL from inside the cluster. The handler refuses private
	// and link-local targets; the role check is the second half of that.
	r.With(hubauth.RequireRole(hubauth.RoleOwner)).Post("/api/webhooks", webhookAPIHandler.CreateWebhook)
	r.With(hubauth.RequireRole(hubauth.RoleOwner)).Delete("/api/webhooks/{id}", webhookAPIHandler.DeleteWebhook)
	r.Get("/api/webhooks/logs", webhookAPIHandler.GetLogs)
	// MPTCP concentrator API
	mptcpHandler := mptcp.NewAPIHandler(mptcpMonitor)
	r.Get("/api/mptcp/status", mptcpHandler.GetStatus)
	r.Put("/api/mptcp/strategy", mptcpHandler.SetStrategy)
	r.Get("/api/mptcp/endpoints", mptcpHandler.ListEndpoints)
	r.Post("/api/mptcp/endpoints", mptcpHandler.AddEndpointHandler)
	r.Delete("/api/mptcp/endpoints/{id}", mptcpHandler.RemoveEndpointHandler)

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
		proxy := tak.NewMartiProxy(cfg.TAKHost, 8443, true, cfg.TAKAPIInsecureTLS)
		missions, err := proxy.ListMissions()
		if err != nil {
			if errors.Is(err, tak.ErrMartiUnavailable) {
				// OpenTAKServer answers the Marti mission API with its web UI: the
				// feature is not available on this TAK server, which is a state,
				// not a gateway failure.
				api.WriteJSON(w, http.StatusOK, map[string]any{"missions": []any{}, "available": false, "reason": "the TAK server does not expose the Marti mission API"})
				return
			}
			api.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		api.WriteJSON(w, http.StatusOK, map[string]any{"missions": missions, "available": true})
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
		client := cloudloopPool.ForTenant(r.Context(), hubauth.TenantIDFromContext(r.Context()))
		if client == nil {
			http.Error(w, `{"error":"no Cloudloop account configured for this tenant"}`, http.StatusNotFound)
			return
		}
		balance, err := client.GetCreditBalance(r.Context())
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
			checker.AddInfoProbe("hawkbit", func(ctx context.Context) error {
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
	routeEngine := routing.NewEngine(dataStore, msgBus, tenants)
	// Register SMS destination handler if SMS is enabled.
	// Use the same API-key-authenticated client as the send endpoint. [MESHSAT-448]
	if cfg.SMSEnabled {
		routeEngine.RegisterHandler("sms", routing.NewSMSHandlerPool(smsPool))
	}
	// Register Email destination handler if email is enabled.
	if emailKeyRing != nil {
		routeEmailClient := hubemail.NewClient(cfg.EmailSMTPHost, cfg.EmailFrom, cfg.EmailUsername, cfg.EmailPassword, emailKeyRing)
		routeEngine.RegisterHandler("email", routing.NewEmailHandler(routeEmailClient))
	}
	// Register webhook, notification, MQTT, TAK, and APRS destination handlers.
	// Satellite destination (MESHSAT-964 D): the text goes to a bridge's
	// modem, IMT through the tenant's Cloudloop account for a 9704, Rock7 MT
	// for a 9603; the route filter lists the modem IMEIs.
	routeEngine.RegisterHandler("satellite", routing.NewSatelliteHandler(func(ctx context.Context, tenantID, imei, text string) error {
		if _, imt := thingResolver.Resolve(tenantID, imei); imt {
			_, err := mtSender.SendDirect(imei, cloudloop.MTSendRequest{Text: text})
			return err
		}
		client := rock7Pool.ForTenant(ctx, tenantID)
		if client == nil {
			return fmt.Errorf("no Rock7 account configured for tenant %s", tenantID)
		}
		_, err := client.SendMT(ctx, imei, hex.EncodeToString([]byte(text)))
		return err
	}))
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
			// Unknown API paths must not fall through to the SPA shell.
			if strings.HasPrefix(req.URL.Path, "/api/") {
				api.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
				return
			}
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
	msgScheduler := scheduler.New(dataStore, &scheduledSenderAdapter{rock7: rock7Client, pool: rock7Pool, tenants: tenants}, 30*time.Second)
	go msgScheduler.Run(ctx)

	// Store maintenance for the single-writer claims (MESHSAT-910): drop
	// dispatch claims older than a day and fail scheduled sends whose owner
	// died. Leader-only.
	leaderSingletons.Add("claims-maintenance", func(sctx context.Context) {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
				mctx, cancel := context.WithTimeout(sctx, 30*time.Second)
				if n, err := dataStore.PurgeClaims(mctx, time.Now().Add(-24*time.Hour)); err != nil {
					slog.Warn("maintenance: purge claims", "error", err)
				} else if n > 0 {
					slog.Debug("maintenance: purged claims", "count", n)
				}
				if n, err := dataStore.ExpireStaleSends(mctx, 10*time.Minute); err != nil {
					slog.Warn("maintenance: expire stale sends", "error", err)
				} else if n > 0 {
					slog.Warn("maintenance: expired stale scheduled sends", "count", n)
				}
				cancel()
			}
		}
	})

	// Every leader-only service is registered: run the election. With the
	// Noop elector (standalone) this starts them immediately.
	go leader.RunWith(ctx, leaderElector, leaderSingletons, onLeaderAcquired, onLeaderLost)

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
	// Migrations ran and the listener is up: startup is complete. The startup
	// probe no longer depends on any dependency probe from here on.
	checker.MarkStarted()

	<-ctx.Done()
	slog.Info("shutting down")

	// Fail readiness first so the load balancer stops routing new requests,
	// then give in-flight requests a moment before closing the listener.
	checker.SetDraining()
	if cfg.ShutdownDrainSeconds > 0 {
		time.Sleep(time.Duration(cfg.ShutdownDrainSeconds) * time.Second)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
	close(touchCh) // drain remaining API key last_used updates
	if aprsisClient != nil {
		aprsisClient.Disconnect()
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
	rock7   *rock7.Client
	pool    *rock7.ClientPool // per-tenant accounts (MESHSAT-977)
	tenants *tenancy.Resolver
}

func (a *scheduledSenderAdapter) SendScheduled(ctx context.Context, msg *store.Message) error {
	client := a.rock7
	if a.pool != nil {
		tenantID := store.DefaultTenantID
		if a.tenants != nil {
			tenantID = a.tenants.ForDevice(ctx, msg.DeviceIMEI)
		}
		client = a.pool.ForTenant(ctx, tenantID)
	}
	if client == nil {
		return fmt.Errorf("no Rock7 account configured for the message's tenant")
	}
	dataHex := msg.RawHex
	if dataHex == "" {
		dataHex = hex.EncodeToString([]byte(msg.Text))
	}
	_, err := client.SendMT(ctx, msg.DeviceIMEI, dataHex)
	return err
}

// mqttClientID makes the broker client id unique per replica: an MQTT
// broker keeps one session per client id and NATS replaces the older
// connection ("Replacing old client"), so two pods with the same id kick each
// other off every few seconds and lose subscriptions (seen 2026-09-08 with
// replicas=2, MESHSAT-980). POD_NAME comes from the downward API.
func mqttClientID(base string) string {
	if pod := os.Getenv("POD_NAME"); pod != "" {
		return base + "-" + pod
	}
	if host, err := os.Hostname(); err == nil && host != "" && os.Getenv("HUB_MODE") == "kubernetes" {
		return base + "-" + host
	}
	return base
}

// leaderInstanceID is the identity a replica presents to the leader election:
// the pod name on Kubernetes, else the hostname and pid behind the client id.
func leaderInstanceID(base string) string {
	if pod := os.Getenv("POD_NAME"); pod != "" {
		return pod
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return fmt.Sprintf("%s-%s-%d", base, host, os.Getpid())
	}
	return fmt.Sprintf("%s-%d", base, os.Getpid())
}

// basemapHandler builds the basemap route from configuration, falling back to
// the audit archive's endpoint, bucket, region and keys because both live in
// the same object store. An unset object key disables the route: the map then
// renders its data layers on an empty background and says the basemap is
// unavailable, rather than reaching for a third-party tile host.
func basemapHandler(cfg config.Config) *api.BasemapHandler {
	key := strings.TrimSpace(cfg.BasemapS3Key)
	if key == "" {
		return nil
	}
	pick := func(v, fallback string) string {
		if v != "" {
			return v
		}
		return fallback
	}
	client, err := objstore.New(objstore.Config{
		Endpoint:  pick(cfg.BasemapS3Endpoint, cfg.AuditArchiveS3Endpoint),
		Bucket:    pick(cfg.BasemapS3Bucket, cfg.AuditArchiveS3Bucket),
		Region:    pick(cfg.BasemapS3Region, cfg.AuditArchiveS3Region),
		AccessKey: pick(cfg.BasemapS3AccessKey, cfg.AuditArchiveS3AccessKey),
		SecretKey: pick(cfg.BasemapS3SecretKey, cfg.AuditArchiveS3SecretKey),
	})
	if err != nil {
		slog.Error("basemap: route disabled, object store not configured", "error", err)
		return nil
	}
	maxAge := 24 * time.Hour
	if cfg.BasemapCacheMaxAge != "" {
		d, err := time.ParseDuration(cfg.BasemapCacheMaxAge)
		if err != nil {
			slog.Warn("basemap: bad cache max age, using 24h", "value", cfg.BasemapCacheMaxAge, "error", err)
		} else {
			maxAge = d
		}
	}
	assets := strings.TrimSpace(cfg.BasemapS3AssetPrefix)
	if assets == "" {
		assets = "basemap/assets"
	}
	return api.NewBasemapHandler(client, key, cfg.BasemapS3LocalKey, assets, maxAge)
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

// migrateOnly reports whether the process was asked to stop after migrations.
func migrateOnly() bool {
	for _, a := range os.Args[1:] {
		if a == "--migrate-only" || a == "-migrate-only" {
			return true
		}
	}
	v := strings.ToLower(os.Getenv("HUB_MIGRATE_ONLY"))
	return v == "true" || v == "1"
}
