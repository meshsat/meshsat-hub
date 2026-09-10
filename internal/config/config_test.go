package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.Port != 6070 {
		t.Errorf("expected port 6070, got %d", cfg.Port)
	}
	if cfg.MQTTBrokerURL != "tcp://mqtt:1883" {
		t.Errorf("unexpected MQTT URL: %s", cfg.MQTTBrokerURL)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("unexpected log level: %s", cfg.LogLevel)
	}
	if cfg.LogFormat != "json" {
		t.Errorf("unexpected log format: %s", cfg.LogFormat)
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	t.Setenv("HUB_PORT", "9999")
	t.Setenv("HUB_MQTT_BROKER_URL", "tcp://localhost:1883")
	t.Setenv("HUB_LOG_LEVEL", "DEBUG")
	t.Setenv("HUB_LOG_FORMAT", "TEXT")
	t.Setenv("HUB_AUTH_TOKEN", "secret123")
	t.Setenv("HUB_CONFIG_FILE", "/nonexistent/config.yaml")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != 9999 {
		t.Errorf("expected port 9999, got %d", cfg.Port)
	}
	if cfg.MQTTBrokerURL != "tcp://localhost:1883" {
		t.Errorf("unexpected MQTT URL: %s", cfg.MQTTBrokerURL)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("expected log level debug, got %s", cfg.LogLevel)
	}
	if cfg.LogFormat != "text" {
		t.Errorf("expected log format text, got %s", cfg.LogFormat)
	}
	if cfg.AuthToken != "secret123" {
		t.Errorf("expected auth token secret123, got %s", cfg.AuthToken)
	}
}

func TestLoad_YAMLFile(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	content := []byte("port: 7777\nmqtt_broker_url: tcp://test:1883\nlog_level: warn\n")
	if err := os.WriteFile(yamlPath, content, 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HUB_CONFIG_FILE", yamlPath)
	// Clear any other overrides
	t.Setenv("HUB_PORT", "")
	t.Setenv("HUB_MQTT_BROKER_URL", "")
	t.Setenv("HUB_LOG_LEVEL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != 7777 {
		t.Errorf("expected port 7777, got %d", cfg.Port)
	}
	if cfg.MQTTBrokerURL != "tcp://test:1883" {
		t.Errorf("unexpected MQTT URL: %s", cfg.MQTTBrokerURL)
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("expected log level warn, got %s", cfg.LogLevel)
	}
}

func TestResolvedDBDriver(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"explicit postgres", Config{DBDriver: "postgres", DatabaseURL: "user:pw@tcp(h:3306)/db"}, "postgres"},
		{"explicit sqlite", Config{DBDriver: "sqlite", Mode: "kubernetes"}, "sqlite"},
		{"sniff postgres url", Config{DatabaseURL: "postgres://u:p@h:5432/db?sslmode=require"}, "postgres"},
		{"sniff postgresql url", Config{DatabaseURL: "postgresql://u:p@h/db"}, "postgres"},
		{"mysql dsn is no longer a driver", Config{Mode: "cluster", DatabaseURL: "meshsat:pw@tcp(127.0.0.1:3306)/meshsat_hub?parseTime=true"}, "postgres"},
		{"explicit mariadb ignored", Config{DBDriver: "mariadb", Mode: "standalone"}, "sqlite"},
		{"cluster default", Config{Mode: "cluster"}, "postgres"},
		{"kubernetes default", Config{Mode: "kubernetes"}, "postgres"},
		{"standalone default", Config{Mode: "standalone"}, "sqlite"},
		{"empty", Config{}, "sqlite"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.ResolvedDBDriver(); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestSQLitePathEnv(t *testing.T) {
	t.Setenv("HUB_SQLITE_PATH", "/tmp/x.db")
	t.Setenv("HUB_DB_DRIVER", "Postgres")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SQLitePath != "/tmp/x.db" || cfg.DBDriver != "postgres" {
		t.Errorf("env not applied: %+v", cfg)
	}
}

// The ExternalSecret renders a key missing from the backing store as the
// literal string "<no value>". It is not empty, so every `!= ""` guard in the
// Hub would accept it and boot believing billing was configured while holding a
// credential that cannot work. This has cost this codebase a silent
// misconfiguration once already (MESHSAT-998).
func TestAMissingSecretRendersAsNoValueAndIsRefused(t *testing.T) {
	for _, name := range []string{
		"HUB_STRIPE_SECRET_KEY", "HUB_STRIPE_WEBHOOK_SECRET", "HUB_STRIPE_PATH_SECRET",
	} {
		t.Setenv(name, "<no value>")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.StripeSecretKey != "" || cfg.StripeWebhookSecret != "" || cfg.StripePathSecret != "" {
		t.Fatalf("an unrendered secret was accepted as configured: key=%q whsec=%q path=%q",
			cfg.StripeSecretKey, cfg.StripeWebhookSecret, cfg.StripePathSecret)
	}
}

// And a real value still arrives, so the guard is not just refusing everything.
func TestARealStripeSecretIsAccepted(t *testing.T) {
	t.Setenv("HUB_STRIPE_SECRET_KEY", "sk_test_abc")
	t.Setenv("HUB_STRIPE_WEBHOOK_SECRET", "whsec_abc")
	t.Setenv("HUB_STRIPE_PATH_SECRET", "pathsecret")
	t.Setenv("HUB_STRIPE_PRICES", "price_a=crew, price_b=fleet")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.StripeSecretKey != "sk_test_abc" || cfg.StripeWebhookSecret != "whsec_abc" {
		t.Errorf("secrets not read: %+v", cfg.StripeSecretKey)
	}
	if cfg.StripePrices["price_a"] != "crew" || cfg.StripePrices["price_b"] != "fleet" {
		t.Errorf("prices not parsed: %v", cfg.StripePrices)
	}
}

// The three secrets are separate on purpose. The signing secret authenticates a
// delivery and must never end up in a URL, which travels through consoles, logs
// and support tickets.
func TestThePathSecretIsNotTheSigningSecret(t *testing.T) {
	t.Setenv("HUB_STRIPE_WEBHOOK_SECRET", "whsec_theRealOne")
	t.Setenv("HUB_STRIPE_PATH_SECRET", "someOtherThing")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.StripePathSecret == cfg.StripeWebhookSecret {
		t.Fatal("the path secret and the signing secret are the same value; " +
			"the signing secret would then be in every request URL")
	}
}
