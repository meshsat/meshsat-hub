package middleware

import "testing"

// A webhook path carries the tenant's secret in its last segment, and these
// log lines go to stdout and the cluster's log store. The provider must stay
// visible so the logs remain useful; the secret must not (MESHSAT-975).
func TestRedactWebhookSecret(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"tenant webhook", "/api/webhook/cloudloop/9f3c1d2e4b5a6789", "/api/webhook/cloudloop/{secret}"},
		{"twilio", "/api/webhook/sms/abcdef0123456789", "/api/webhook/sms/{secret}"},
		{"legacy platform path", "/api/webhook/rockblock", "/api/webhook/rockblock"},
		{"trailing slash carries no secret", "/api/webhook/rockblock/", "/api/webhook/rockblock/"},
		{"deeper path still redacts at the first segment", "/api/webhook/email/s3cr3t/extra", "/api/webhook/email/{secret}"},
		{"unrelated path untouched", "/api/devices/300234065123456", "/api/devices/300234065123456"},
		{"root untouched", "/", "/"},
		{"prefix lookalike untouched", "/api/webhooks/logs", "/api/webhooks/logs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactWebhookSecret(tt.in); got != tt.want {
				t.Errorf("redactWebhookSecret(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
