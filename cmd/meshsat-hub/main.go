package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/aprsis"
	"github.com/cubeos-app/meshsat-hub/internal/backup"
	"github.com/cubeos-app/meshsat-hub/internal/cloudloop"
	"github.com/cubeos-app/meshsat-hub/internal/config"
	"github.com/cubeos-app/meshsat-hub/internal/health"
	hubmqtt "github.com/cubeos-app/meshsat-hub/internal/mqtt"
	"github.com/cubeos-app/meshsat-hub/internal/ratelimit"
	"github.com/cubeos-app/meshsat-hub/internal/rockblock"
	"github.com/cubeos-app/meshsat-hub/internal/tak"
	"github.com/cubeos-app/meshsat-hub/internal/webhook"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

var version = "dev"

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	initLogger(cfg)
	slog.Info("starting meshsat-hub", "version", version, "port", cfg.Port)

	checker := health.New()

	// Connect to MQTT broker.
	mqttClient := hubmqtt.New(cfg.MQTTBrokerURL, cfg.MQTTClientID)
	if err := mqttClient.Connect(); err != nil {
		slog.Warn("mqtt connection failed (will retry in background)", "error", err)
	}
	checker.Set("mqtt", mqttClient.IsConnected())

	// Cloudloop API client for MT sends.
	cloudloopClient := cloudloop.NewClient(cfg.CloudloopAPIURL, cfg.CloudloopAPIKey)

	// Per-device rate limiter for MT sends.
	limiter := ratelimit.NewDeviceLimiter(
		float64(cfg.RateLimitBurst),  // max burst
		cfg.RateLimitRefillPerMin/60, // tokens per second
		cfg.RateLimitDailyCap,        // daily cap
		mqttClient,                   // for MQTT alerts
	)
	rateLimitHandler := ratelimit.NewHandler(limiter)

	// Start MT sender (subscribes to meshsat/+/mt/send).
	mtSender := cloudloop.NewSender(cloudloopClient, mqttClient)
	mtSender.SetRateLimiter(limiter)
	if mqttClient.IsConnected() {
		if err := mtSender.Start(); err != nil {
			slog.Error("failed to start MT sender", "error", err)
		}
	}

	// TAK/CoT gateway (optional — subscribe to MQTT, forward to OpenTAKServer).
	var takClient *tak.Client
	if cfg.TAKEnabled && cfg.TAKHost != "" {
		takPort := cfg.TAKPort
		if takPort == 0 {
			takPort = 8087
		}
		takClient = tak.NewClient(cfg.TAKHost, takPort, cfg.TAKSSL)
		if err := takClient.Connect(); err != nil {
			slog.Warn("tak: connection failed (will not forward CoT)", "error", err)
		} else if mqttClient.IsConnected() {
			takSub := tak.NewSubscriber(mqttClient, takClient, cfg.TAKCallsignPrefix, cfg.TAKCotStaleSec)
			if err := takSub.Start(); err != nil {
				slog.Error("tak: failed to start subscriber", "error", err)
			}
		}
	}

	// APRS-IS IGate (optional — inject satellite positions into APRS-IS network).
	var aprsisClient *aprsis.Client
	if cfg.APRSISEnabled && cfg.APRSISCallsign != "" && cfg.APRSISPasscode != "" {
		server := cfg.APRSISServer
		if server == "" {
			server = "euro.aprs2.net:14580"
		}
		aprsisClient = aprsis.NewClient(server, cfg.APRSISCallsign, 10, cfg.APRSISPasscode, "")
		if err := aprsisClient.Connect(); err != nil {
			slog.Warn("aprsis: connection failed (will not inject positions)", "error", err)
		} else if mqttClient.IsConnected() {
			aprsisSub := aprsis.NewSubscriber(mqttClient, aprsisClient, 60)
			if err := aprsisSub.Start(); err != nil {
				slog.Error("aprsis: failed to start subscriber", "error", err)
			}
		}
	}

	// Outbound webhook dispatcher (fires on MO, SOS, position, telemetry, MT status).
	webhookDispatcher := webhook.NewDispatcher(mqttClient)
	webhookAPIHandler := webhook.NewAPIHandler(webhookDispatcher)
	if mqttClient.IsConnected() {
		if err := webhookDispatcher.Start(mqttClient); err != nil {
			slog.Error("webhook: failed to start dispatcher", "error", err)
		}
	}

	// RockBLOCK webhook handler.
	rbHandler := rockblock.NewHandler(mqttClient, cfg.RockBLOCKSecret)

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)
	r.Get("/healthz", health.LivezHandler)
	r.Get("/readyz", checker.ReadyzHandler)
	r.Post("/api/webhook/rockblock", rbHandler.ServeHTTP)
	r.Get("/api/ratelimit", rateLimitHandler.GetAllUsage)
	r.Get("/api/ratelimit/{deviceID}", rateLimitHandler.GetUsage)
	r.Post("/api/ratelimit/{deviceID}/override", rateLimitHandler.PostOverride)
	r.Delete("/api/ratelimit/{deviceID}/override", rateLimitHandler.DeleteOverride)
	r.Get("/api/webhooks", webhookAPIHandler.ListWebhooks)
	r.Post("/api/webhooks", webhookAPIHandler.CreateWebhook)
	r.Delete("/api/webhooks/{id}", webhookAPIHandler.DeleteWebhook)
	r.Get("/api/webhooks/logs", webhookAPIHandler.GetLogs)

	// Backup/restore
	backupProvider := &backup.HubStateProvider{Config: cfg, WebhookLister: webhookDispatcher}
	backupHandler := backup.NewAPIHandler(backupProvider, "/data")
	r.Get("/api/backup/export", backupHandler.ExportBackup)
	r.Post("/api/backup/diff", backupHandler.DiffBackup)
	r.Post("/api/backup/import", backupHandler.ImportBackup)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
	mqttClient.Disconnect()
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
