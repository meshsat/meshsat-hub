package config

import "testing"

func TestLegacyTokenSwitch(t *testing.T) {
	t.Setenv("HUB_AUTH_TOKEN", "static-secret")
	t.Setenv("HUB_JWT_SIGNING_KEY", "k")

	t.Setenv("HUB_LEGACY_TOKEN_ENABLED", "")
	if c, err := Load(); err != nil || c.AuthToken != "static-secret" {
		t.Fatalf("default must keep the token: tok=%q err=%v", c.AuthToken, err)
	}
	t.Setenv("HUB_LEGACY_TOKEN_ENABLED", "true")
	if c, _ := Load(); c.AuthToken != "static-secret" {
		t.Fatalf("true must keep the token: %q", c.AuthToken)
	}
	t.Setenv("HUB_LEGACY_TOKEN_ENABLED", "false")
	if c, err := Load(); err != nil || c.AuthToken != "" {
		t.Fatalf("false must clear the token: tok=%q err=%v", c.AuthToken, err)
	}
	t.Setenv("HUB_LEGACY_TOKEN_ENABLED", "banana")
	if _, err := Load(); err == nil {
		t.Fatal("a non-boolean must be refused")
	}
	t.Setenv("HUB_LEGACY_TOKEN_ENABLED", "false")
	t.Setenv("HUB_JWT_SIGNING_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("clearing the ONLY credential must refuse to boot")
	}
}
