package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/config"
)

// The TAK gateway is the platform's: a customer's integrations list must not
// carry the operator's TAK server address or federation peers. [MESHSAT-1032]
func TestIntegrationsShowThePlatformTAKOnlyToPlatformAdmins(t *testing.T) {
	h := NewIntegrationHandler(config.Config{
		TAKEnabled: true, TAKHost: "192.168.192.10", TAKPort: 8088,
		TAKFederationEnabled: true, TAKFederationPeers: []string{"192.168.15.10:9001"},
	})

	tests := []struct {
		name    string
		user    *hubauth.User
		wantTAK bool
	}{
		{"customer", &hubauth.User{}, false},
		{"no user", nil, false},
		{"platform admin", &hubauth.User{PlatformAdmin: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/integrations", nil)
			if tt.user != nil {
				req = req.WithContext(context.WithValue(req.Context(), hubauth.UserContextKey, tt.user))
			}
			rec := httptest.NewRecorder()
			h.ListIntegrations(rec, req)

			body := rec.Body.String()
			for _, platformDetail := range []string{"192.168.192.10", "192.168.15.10", "TAK (CoT Gateway)"} {
				if got := strings.Contains(body, platformDetail); got != tt.wantTAK {
					t.Errorf("response contains %q = %v, want %v", platformDetail, got, tt.wantTAK)
				}
			}
			if !strings.Contains(body, "Rock7") {
				t.Error("the tenant-facing integrations went missing too")
			}
		})
	}
}
