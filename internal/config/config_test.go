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
		{"sniff mysql dsn", Config{Mode: "cluster", DatabaseURL: "meshsat:pw@tcp(127.0.0.1:3306)/meshsat_hub?parseTime=true"}, "mariadb"},
		{"cluster default", Config{Mode: "cluster"}, "mariadb"},
		{"kubernetes default", Config{Mode: "kubernetes"}, "mariadb"},
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
