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

// A claim path carries its bearer nonce in the last segment. Since a
// provisioning claim can answer 503 and stay claimable (MESHSAT-1298), a logged
// claim path is a live token in the log store; it must not be written. The
// neighbouring routes of the same shape stay readable.
func TestRedactClaimNonce(t *testing.T) {
	const n = "54042487dacce0fefb33b76807e2acaa"
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"provisioning claim", "/api/bridges/kit-a/provision/" + n, "/api/bridges/kit-a/provision/{nonce}"},
		{"tak enrolment claim", "/api/tak/enroll/0123456789abcdef0123456789abcdef/" + n, "/api/tak/enroll/0123456789abcdef0123456789abcdef/{nonce}"},
		{"qr route readable", "/api/bridges/kit-a/provision/qr", "/api/bridges/kit-a/provision/qr"},
		{"status route readable", "/api/bridges/kit-a/provision/status", "/api/bridges/kit-a/provision/status"},
		{"not nonce shaped", "/api/bridges/kit-a/provision/NOTHEX0123456789abcdef0123456789", "/api/bridges/kit-a/provision/NOTHEX0123456789abcdef0123456789"},
		{"other route untouched", "/api/bridges/kit-a/commands/" + n, "/api/bridges/kit-a/commands/" + n},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactClaimNonce(tt.in); got != tt.want {
				t.Errorf("redactClaimNonce(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
