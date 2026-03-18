package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cubeos-app/meshsat-hub/cmd/meshsat-hub/web"
	"github.com/cubeos-app/meshsat-hub/internal/api"
	"github.com/cubeos-app/meshsat-hub/internal/aprsis"
	"github.com/cubeos-app/meshsat-hub/internal/audit"
	hubauth "github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/backup"
	"github.com/cubeos-app/meshsat-hub/internal/bus"
	"github.com/cubeos-app/meshsat-hub/internal/bus/paho"
	"github.com/cubeos-app/meshsat-hub/internal/cloudloop"
	"github.com/cubeos-app/meshsat-hub/internal/config"
	"github.com/cubeos-app/meshsat-hub/internal/dedup"
	"github.com/cubeos-app/meshsat-hub/internal/health"
	"github.com/cubeos-app/meshsat-hub/internal/leader"
	"github.com/cubeos-app/meshsat-hub/internal/ratelimit"
	"github.com/cubeos-app/meshsat-hub/internal/rockblock"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/cubeos-app/meshsat-hub/internal/store/postgres"
	"github.com/cubeos-app/meshsat-hub/internal/store/sqlite"
	"github.com/cubeos-app/meshsat-hub/internal/tak"
	"github.com/cubeos-app/meshsat-hub/internal/webhook"
	"github.com/cubeos-app/meshsat-hub/internal/wireguard"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/redis/go-redis/v9"
)

var version = "dev"

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
	checker.Set("mqtt", msgBus.IsConnected())

	// --- Store (tri-mode) ---
	var dataStore store.Store
	switch cfg.Mode {
	case "cluster", "kubernetes":
		pgStore, err := postgres.New(ctx, cfg.DatabaseURL)
		if err != nil {
			slog.Error("postgres connection failed", "error", err)
			os.Exit(1)
		}
		if err := pgStore.Migrate(ctx); err != nil {
			slog.Error("postgres migration failed", "error", err)
			os.Exit(1)
		}
		dataStore = pgStore
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
	default:
		dedupTracker = dedup.NewMemoryDedup(1 * time.Hour)
	}
	_ = dedupTracker // will be wired into MO ingestion pipeline

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
		limiter = ratelimit.NewRedisLimiter(redisClient, cfg.RateLimitDailyCap)
	default:
		limiter = ratelimit.NewDeviceLimiter(
			float64(cfg.RateLimitBurst),  // max burst
			cfg.RateLimitRefillPerMin/60, // tokens per second
			cfg.RateLimitDailyCap,        // daily cap
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
	default: // "standalone", "kubernetes" (k8s Lease not implemented yet)
		leaderElector = leader.NewNoop()
	}

	// Cloudloop API client for MT sends.
	cloudloopClient := cloudloop.NewClient(cfg.CloudloopAPIURL, cfg.CloudloopAPIKey)

	// Start MT sender (subscribes to meshsat/+/mt/send).
	mtSender := cloudloop.NewSender(cloudloopClient, msgBus)
	mtSender.SetRateLimiter(limiter)
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

	// RockBLOCK webhook handler.
	rbHandler := rockblock.NewHandler(msgBus, cfg.RockBLOCKSecret)
	rbHandler.SetAudit(auditSvc)

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)

	// API key middleware — runs first; if bearer is a meshsat_ key, validates and sets
	// user+tenant in context. Non-API-key tokens pass through to JWT/token middleware.
	apiKeyValidator := func(ctx context.Context, keyHash string) (*hubauth.User, string, error) {
		k, tenantID, err := dataStore.GetAPIKeyByHash(ctx, keyHash)
		if err != nil {
			return nil, "", err
		}
		// Touch last_used asynchronously.
		go func() { _ = dataStore.TouchAPIKeyLastUsed(context.Background(), k.ID) }()
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
		} else if cfg.AuthToken != "" {
			authMode = "token"
		} else {
			authMode = "none"
		}
	}
	r.Use(hubauth.Middleware(hubauth.Config{
		Mode:          authMode,
		Token:         cfg.AuthToken,
		OIDCIssuerURL: cfg.OIDCIssuerURL,
		OIDCAudience:  cfg.OIDCAudience,
	}))
	// Tenant isolation middleware — resolves tenant from JWT claim / X-Tenant-ID header / default.
	// Enforce mode disabled for backward compatibility; enable via HUB_TENANT_ENFORCE=true.
	r.Use(hubauth.TenantMiddleware(cfg.TenantEnforce))

	r.Get("/healthz", health.LivezHandler)
	r.Get("/readyz", checker.ReadyzHandler)
	r.Post("/api/webhook/rockblock", rbHandler.ServeHTTP)

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
	r.Get("/api/credits", func(w http.ResponseWriter, r *http.Request) {
		balance, err := cloudloopClient.GetCreditBalance(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(balance)
	})

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

	// WireGuard peer management (optional)
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
			slog.Info("wireguard: peer management enabled", "url", cfg.WGURL)
		}
	}

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
