package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cubeos-app/meshsat-hub/cmd/meshsat-hub/web"
	"github.com/cubeos-app/meshsat-hub/internal/api"
	"github.com/cubeos-app/meshsat-hub/internal/apprise"
	"github.com/cubeos-app/meshsat-hub/internal/aprsis"
	"github.com/cubeos-app/meshsat-hub/internal/astrocast"
	"github.com/cubeos-app/meshsat-hub/internal/audit"
	hubauth "github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/backup"
	"github.com/cubeos-app/meshsat-hub/internal/bus"
	"github.com/cubeos-app/meshsat-hub/internal/bus/paho"
	"github.com/cubeos-app/meshsat-hub/internal/cloudloop"
	"github.com/cubeos-app/meshsat-hub/internal/cluster"
	"github.com/cubeos-app/meshsat-hub/internal/config"
	"github.com/cubeos-app/meshsat-hub/internal/constellation"
	hubcrypto "github.com/cubeos-app/meshsat-hub/internal/crypto"
	"github.com/cubeos-app/meshsat-hub/internal/deadman"
	"github.com/cubeos-app/meshsat-hub/internal/dedup"
	hubemail "github.com/cubeos-app/meshsat-hub/internal/email"
	"github.com/cubeos-app/meshsat-hub/internal/escalation"
	"github.com/cubeos-app/meshsat-hub/internal/fragment"
	"github.com/cubeos-app/meshsat-hub/internal/hawkbit"
	"github.com/cubeos-app/meshsat-hub/internal/health"
	"github.com/cubeos-app/meshsat-hub/internal/leader"
	"github.com/cubeos-app/meshsat-hub/internal/mptcp"
	"github.com/cubeos-app/meshsat-hub/internal/ntfy"
	"github.com/cubeos-app/meshsat-hub/internal/position"
	"github.com/cubeos-app/meshsat-hub/internal/ratelimit"
	"github.com/cubeos-app/meshsat-hub/internal/rockblock"
	"github.com/cubeos-app/meshsat-hub/internal/routing"
	"github.com/cubeos-app/meshsat-hub/internal/sms"
	"github.com/cubeos-app/meshsat-hub/internal/sos"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/cubeos-app/meshsat-hub/internal/store/mariadb"
	"github.com/cubeos-app/meshsat-hub/internal/store/sqlite"
	"github.com/cubeos-app/meshsat-hub/internal/tak"
	hubtor "github.com/cubeos-app/meshsat-hub/internal/tor"
	"github.com/cubeos-app/meshsat-hub/internal/webhook"
	"github.com/cubeos-app/meshsat-hub/internal/wireguard"

	swaggerDocs "github.com/cubeos-app/meshsat-hub/docs/swagger"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/redis/go-redis/v9"
)

var version = "dev"

// @title        MeshSat Hub API
// @version      1.1
// @description  Multi-tenant SaaS platform for satellite device management. Ingests MO messages from Iridium/Astrocast, manages devices, SOS escalation, dead man's switch, and E2E encryption.
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	checker := health.New()

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
		dbStore, err := mariadb.New(cfg.DatabaseURL)
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
		sqlStore, err := sqlite.New("/data/hub.db")
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

	// Start MT sender (subscribes to meshsat/+/mt/send).
	mtSender := cloudloop.NewSender(cloudloopClient, msgBus)
	mtSender.SetRateLimiter(limiter)
	mtSender.SetAudit(auditSvc)
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

	// Astrocast API client (optional — second satellite constellation).
	var astrocastClient *astrocast.Client
	if cfg.AstrocastAPIKey != "" {
		astrocastClient = astrocast.NewClient(cfg.AstrocastAPIURL, cfg.AstrocastAPIKey)
		slog.Info("astrocast: API client enabled", "url", cfg.AstrocastAPIURL)
	}

	// Constellation router — multi-backend satellite send.
	constellationRouter := constellation.NewRouter(constellation.StrategyAvailable)
	constellationRouter.Register(constellation.NewIridiumBackend(cloudloopClient))
	if astrocastClient != nil {
		constellationRouter.Register(constellation.NewAstrocastBackend(astrocastClient))
	}

	// MPTCP concentrator monitor (aggregates satellite + cellular links).
	mptcpMonitor := mptcp.NewMonitor(30*time.Second, msgBus)
	go mptcpMonitor.Start(ctx)

	// TAK/CoT gateway and APRS-IS IGate are singletons — run inside leader election callback.
	var takClient *tak.Client
	var aprsisClient *aprsis.Client

	go leaderElector.Run(ctx, func() {
		// onAcquired: start singleton services
		slog.Info("leader acquired — starting TAK and APRS-IS")

		// TAK/CoT gateway (optional — subscribe to MQTT, forward to OpenTAKServer).
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
				}
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
		slog.Info("leader lost — stopping TAK and APRS-IS")
		if aprsisClient != nil {
			aprsisClient.Disconnect()
			aprsisClient = nil
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

	// RockBLOCK webhook handler.
	rbHandler := rockblock.NewHandler(msgBus, cfg.RockBLOCKSecret)
	rbHandler.SetAudit(auditSvc)
	rbHandler.SetDedup(dedupTracker)
	rbHandler.SetReassembler(reassembler)
	rbHandler.SetKeyStore(keyStore)
	rbHandler.SetDeadman(deadmanMonitor)

	// Astrocast MO webhook handler.
	acHandler := astrocast.NewHandler(msgBus, cfg.AstrocastWebhookSecret)
	acHandler.SetAudit(auditSvc)
	acHandler.SetDedup(dedupTracker)
	acHandler.SetReassembler(reassembler)
	acHandler.SetKeyStore(keyStore)
	acHandler.SetDeadman(deadmanMonitor)

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)
	r.Use(api.SecurityHeaders)

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
		Mode:          authMode,
		Token:         cfg.AuthToken,
		OIDCIssuerURL: cfg.OIDCIssuerURL,
		OIDCAudience:  cfg.OIDCAudience,
		JWTSecret:     jwtSecret,
	}))
	// Tenant isolation middleware — resolves tenant from JWT claim / X-Tenant-ID header / default.
	// Enforce mode disabled for backward compatibility; enable via HUB_TENANT_ENFORCE=true.
	r.Use(hubauth.TenantMiddleware(cfg.TenantEnforce))

	r.Get("/healthz", health.LivezHandler)
	r.Get("/readyz", checker.ReadyzHandler)
	r.Post("/api/webhook/rockblock", rbHandler.ServeHTTP)

	r.Post("/api/webhook/astrocast", acHandler.ServeHTTP)

	// SMS gateway (optional — inbound webhook + outbound subscriber)
	if cfg.SMSEnabled && cfg.SMSAccountSID != "" {
		smsClient := sms.NewClient(cfg.SMSAccountSID, cfg.SMSAuthToken, cfg.SMSFromNumber)
		smsWebhook := sms.NewWebhookHandler(msgBus, cfg.SMSWebhookSecret)
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

	// Email gateway routes (PGP key management + inbound webhook)
	if emailKeyRing != nil {
		emailWebhook := hubemail.NewWebhookHandler(msgBus, emailKeyRing)
		r.Post("/api/webhook/email", emailWebhook.ServeHTTP)

		emailAPIHandler := hubemail.NewAPIHandler(emailKeyRing)
		r.Get("/api/email/keys/public", emailAPIHandler.GetPublicKey)
		r.Get("/api/email/keys", emailAPIHandler.ListContacts)
		r.Post("/api/email/keys", emailAPIHandler.AddContact)
		r.Delete("/api/email/keys/{email}", emailAPIHandler.DeleteContact)
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

	// Device registry API
	deviceHandler := api.NewDeviceHandler(dataStore)
	r.Get("/api/devices", deviceHandler.ListDevices)
	r.Post("/api/devices", deviceHandler.CreateDevice)
	r.Get("/api/devices/{imei}", deviceHandler.GetDevice)
	r.Put("/api/devices/{imei}", deviceHandler.UpdateDevice)
	r.Delete("/api/devices/{imei}", deviceHandler.DeleteDevice)

	// Device config versioning
	configHandler := api.NewDeviceConfigHandler(dataStore)
	r.Get("/api/devices/{imei}/config", configHandler.GetLatest)
	r.Put("/api/devices/{imei}/config", configHandler.CreateVersion)
	r.Get("/api/devices/{imei}/config/history", configHandler.ListVersions)
	r.Get("/api/devices/{imei}/config/{version}", configHandler.GetVersion)

	// Device encryption key management
	deviceKeyHandler := api.NewDeviceKeyHandler(dataStore, keyStore)
	r.Post("/api/devices/{imei}/keys", deviceKeyHandler.CreateKey)
	r.Get("/api/devices/{imei}/keys", deviceKeyHandler.ListKeys)
	r.Delete("/api/devices/{imei}/keys/{id}", deviceKeyHandler.DeleteKey)

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

	// Tor .onion address discovery
	torHostPath := os.Getenv("HUB_TOR_HOSTNAME_PATH")
	if torHostPath == "" {
		torHostPath = "/var/lib/tor/hidden_service/hostname"
	}
	torService := hubtor.NewService(torHostPath)
	torHandler := hubtor.NewAPIHandler(torService)
	r.Get("/api/tor/onion", torHandler.GetOnion)

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
	if cfg.SMSEnabled && cfg.SMSAccountSID != "" {
		routeSMSClient := sms.NewClient(cfg.SMSAccountSID, cfg.SMSAuthToken, cfg.SMSFromNumber)
		routeEngine.RegisterHandler("sms", routing.NewSMSHandler(routeSMSClient))
	}
	// Register Email destination handler if email is enabled.
	if emailKeyRing != nil {
		routeEmailClient := hubemail.NewClient(cfg.EmailSMTPHost, cfg.EmailFrom, cfg.EmailUsername, cfg.EmailPassword, emailKeyRing)
		routeEngine.RegisterHandler("email", routing.NewEmailHandler(routeEmailClient))
	}
	if msgBus.IsConnected() {
		_ = routing.SeedDefaults(ctx, dataStore, store.DefaultTenantID)
		if err := routeEngine.Start(); err != nil {
			slog.Error("routing: failed to start engine", "error", err)
		}
	}
	routeAPIHandler := routing.NewAPIHandler(dataStore, routeEngine)
	r.Get("/api/routes", routeAPIHandler.ListRoutes)
	r.Post("/api/routes", routeAPIHandler.CreateRoute)
	r.Get("/api/routes/{id}", routeAPIHandler.GetRoute)
	r.Put("/api/routes/{id}", routeAPIHandler.UpdateRoute)
	r.Delete("/api/routes/{id}", routeAPIHandler.DeleteRoute)

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
	if takClient != nil {
		takClient.Disconnect()
	}
	msgBus.Disconnect()
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
