// Command tak-operator reconciles hosted per-tenant OpenTAKServer instances.
//
// It ships in the same image as the Hub and is selected by the Deployment's
// command, the way the OpenTAKServer image runs three different programs. One
// image means one build, one digest, one CVE scan, and — the reason that matters
// here — the operator and the Hub are always the same commit, so the custom
// resources they share cannot drift apart.
//
// It is a separate PROCESS from the Hub on purpose. Whoever holds a tenant's CA
// key can impersonate every phone in that tenant, so the key lives here, in
// something with no public listener, and the Hub obtains certificates by asking.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/meshsat/meshsat-hub/internal/takoperator"
)

// version is stamped at build time with -X main.version, like the Hub's.
var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level()}))
	slog.SetDefault(logger)

	client, err := takoperator.NewInClusterClient()
	if err != nil {
		logger.Error("tak-operator: cannot reach the Kubernetes API", "error", err)
		os.Exit(1)
	}

	wrapKey, err := takoperator.ParseWrapKey(os.Getenv("TAK_CA_WRAP_KEY"))
	if err != nil {
		// Refusing to start is the point. An operator without a wrap key would
		// generate tenant CA keys it cannot protect and write them to etcd in
		// the clear, and nobody would notice until a backup leaked.
		logger.Error("tak-operator: REFUSING TO START without a CA wrap key. "+
			"Tenant CA keys would be written unwrapped into Secrets, which this "+
			"cluster stores unencrypted in etcd and Velero copies to the object "+
			"store. Set TAK_CA_WRAP_KEY from ci-no/apps/meshsat-hub/tak.",
			"error", err)
		os.Exit(1)
	}

	r := &takoperator.Reconciler{
		Client:      client,
		Namespace:   takoperator.Namespace(),
		DBNamespace: env("TAK_DB_NAMESPACE", takoperator.DefaultDBNamespace),
		DBCluster:   env("TAK_DB_CLUSTER", takoperator.DefaultDBCluster),
		OTSImage:    os.Getenv("TAK_OTS_IMAGE"),
		RabbitImage: os.Getenv("TAK_RABBITMQ_IMAGE"),
		NginxImage:  os.Getenv("TAK_NGINX_IMAGE"),
		WrapKey:     wrapKey,
		Log:         logger,
	}
	if err := r.Validate(); err != nil {
		logger.Error("tak-operator: configuration is incomplete", "error", err)
		os.Exit(1)
	}

	logger.Info("tak-operator: starting",
		"version", version,
		"namespace", r.Namespace,
		"db_namespace", r.DBNamespace,
		"db_cluster", r.DBCluster,
		// The images are logged because which digest is running is the first
		// question asked when an instance misbehaves, and reading it from a
		// ConfigMap that may have changed since the pod started is not an answer.
		"ots_image", r.OTSImage,
		"rabbitmq_image", r.RabbitImage,
		"nginx_image", r.NginxImage)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	r.Run(ctx, interval())

	// A reconcile in flight finishes before the process leaves, so a rollout
	// cannot abandon a half-provisioned instance: Run returns only once the
	// current pass is done.
	logger.Info("tak-operator: stopped")
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func interval() time.Duration {
	if v := os.Getenv("TAK_RECONCILE_INTERVAL_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
		slog.Warn("tak-operator: TAK_RECONCILE_INTERVAL_SEC is not a positive number, using the default", "value", v)
	}
	return 30 * time.Second
}

func level() slog.Level {
	switch os.Getenv("TAK_LOG_LEVEL") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
