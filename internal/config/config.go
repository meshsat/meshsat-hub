package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds all configuration for the MeshSat Hub service.
type Config struct {
	Port            int    `yaml:"port"`
	MQTTBrokerURL   string `yaml:"mqtt_broker_url"`
	MQTTClientID    string `yaml:"mqtt_client_id"`
	RockBLOCKSecret string `yaml:"rockblock_secret"`
	CloudloopAPIKey string `yaml:"cloudloop_api_key"`
	CloudloopAPIURL string `yaml:"cloudloop_api_url"`
	DeviceIMEI      string `yaml:"device_imei"`
	LogLevel        string `yaml:"log_level"`
	LogFormat       string `yaml:"log_format"`
	AuthToken       string `yaml:"auth_token"`
}

// Defaults returns a Config with sensible default values.
func Defaults() Config {
	return Config{
		Port:            6070,
		MQTTBrokerURL:   "tcp://mqtt:1883",
		MQTTClientID:    "meshsat-hub",
		CloudloopAPIURL: "https://api.cloudloop.com",
		LogLevel:        "info",
		LogFormat:       "json",
	}
}

// Load reads configuration from a YAML file (if it exists) and applies
// environment variable overrides. Environment variables use the HUB_ prefix.
func Load() (Config, error) {
	cfg := Defaults()

	// Read YAML file if specified or default exists.
	path := envOr("HUB_CONFIG_FILE", "config.yaml")
	data, err := os.ReadFile(path)
	if err == nil {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	// Environment variable overrides.
	if v := os.Getenv("HUB_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Port = p
		}
	}
	if v := os.Getenv("HUB_MQTT_BROKER_URL"); v != "" {
		cfg.MQTTBrokerURL = v
	}
	if v := os.Getenv("HUB_MQTT_CLIENT_ID"); v != "" {
		cfg.MQTTClientID = v
	}
	if v := os.Getenv("HUB_ROCKBLOCK_SECRET"); v != "" {
		cfg.RockBLOCKSecret = v
	}
	if v := os.Getenv("HUB_CLOUDLOOP_API_KEY"); v != "" {
		cfg.CloudloopAPIKey = v
	}
	if v := os.Getenv("HUB_CLOUDLOOP_API_URL"); v != "" {
		cfg.CloudloopAPIURL = v
	}
	if v := os.Getenv("HUB_DEVICE_IMEI"); v != "" {
		cfg.DeviceIMEI = v
	}
	if v := os.Getenv("HUB_LOG_LEVEL"); v != "" {
		cfg.LogLevel = strings.ToLower(v)
	}
	if v := os.Getenv("HUB_LOG_FORMAT"); v != "" {
		cfg.LogFormat = strings.ToLower(v)
	}
	if v := os.Getenv("HUB_AUTH_TOKEN"); v != "" {
		cfg.AuthToken = v
	}

	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
