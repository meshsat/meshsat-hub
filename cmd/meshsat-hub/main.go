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

	"github.com/cubeos-app/meshsat-hub/internal/cloudloop"
	"github.com/cubeos-app/meshsat-hub/internal/config"
	"github.com/cubeos-app/meshsat-hub/internal/health"
	hubmqtt "github.com/cubeos-app/meshsat-hub/internal/mqtt"
	"github.com/cubeos-app/meshsat-hub/internal/rockblock"
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

	// Start MT sender (subscribes to meshsat/+/mt/send).
	mtSender := cloudloop.NewSender(cloudloopClient, mqttClient)
	if mqttClient.IsConnected() {
		if err := mtSender.Start(); err != nil {
			slog.Error("failed to start MT sender", "error", err)
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
