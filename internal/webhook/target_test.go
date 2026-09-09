package webhook

import (
	"errors"
	"strings"
	"testing"
)

// An outbound webhook makes the Hub fetch a caller-supplied URL from inside the
// cluster, next to the database, the broker, the object store and a cloud
// metadata service. These are the targets that must never be accepted.
func TestValidateTarget_RefusesInternalTargets(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1/",
		"http://localhost:6070/api/tenant",
		"https://LOCALHOST/x",
		"http://[::1]/",
		"http://169.254.169.254/latest/meta-data/", // cloud metadata
		"http://10.2.0.1/",                         // pod network
		"http://192.168.192.10:8880/api/markers",   // the OTS server
		"http://172.16.0.5/",
		"http://meshsat-hub-main-rw.meshsat-hub-db.svc/",
		"http://nats.meshsat-hub.svc.cluster.local:1883/",
		"http://redis.internal/",
		"http://printer.local/",
		"file:///etc/passwd",
		"gopher://127.0.0.1:6379/_FLUSHALL",
		"http://100.64.0.1/", // CGNAT / cluster mesh
		"http://0.0.0.0/",
		"not a url at all",
		"http:///nohost",
	} {
		if err := ValidateTarget(raw); err == nil {
			t.Errorf("accepted an unsafe target: %s", raw)
		} else if !errors.Is(err, ErrUnsafeTarget) {
			t.Errorf("%s: error should wrap ErrUnsafeTarget, got %v", raw, err)
		}
	}
}

// A real customer endpoint must still work, or the control is useless.
func TestValidateTarget_AcceptsPublicTargets(t *testing.T) {
	for _, raw := range []string{
		"https://example.com/hook",
		"https://hooks.slack.com/services/T000/B000/xxxx",
		"http://93.184.216.34/hook", // a public literal
	} {
		if err := ValidateTarget(raw); err != nil {
			// Resolution failures in a sandboxed test runner are not the
			// property under test; only a wrong verdict on a resolvable name is.
			if strings.Contains(err.Error(), "cannot resolve") {
				t.Skipf("no DNS in this environment: %v", err)
			}
			t.Errorf("refused a legitimate target %s: %v", raw, err)
		}
	}
}
