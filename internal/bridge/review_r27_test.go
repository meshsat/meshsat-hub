package bridge

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/protocol"
)

func TestReviewR27DeclarationSurvivesIncompleteBirth(t *testing.T) {
	const imei = "300000000000091"
	for _, scenario := range []string{"late_imei", "gateway_absent"} {
		t.Run(scenario, func(t *testing.T) {
			ms := newMockStore()
			sub := NewSubscriber(newMockBus(), ms, nil)
			birth := protocol.BridgeBirth{
				Protocol:  protocol.ProtocolVersion,
				BridgeID:  "review-kit",
				TenantID:  "default",
				Timestamp: time.Now(),
				Interfaces: []protocol.InterfaceInfo{{
					Name: "iridium_imt", Type: "iridium_imt", Status: "online",
					IMEI: imei, EgressSealed: true,
				}},
			}
			publish := func() {
				payload, err := json.Marshal(birth)
				if err != nil {
					t.Fatal(err)
				}
				sub.handleBridgeBirth("meshsat/bridge/review-kit/birth", payload)
			}
			publish()
			d := NewEgressDeclarations(ms)
			if !d.Sealed(context.Background(), "default", imei) {
				t.Fatal("initial declaration was not installed")
			}
			if scenario == "late_imei" {
				birth.Interfaces[0].IMEI = ""
			} else {
				birth.Interfaces = nil
			}
			publish()
			sealed := d.Sealed(context.Background(), "default", imei)
			t.Logf("scenario=%s owner=%q declaration_after_birth=%v", scenario, ms.deviceBridgeMap[imei], sealed)
			if !sealed {
				t.Fatal("an incomplete inventory erased the modem's known sealing declaration")
			}
		})
	}
}
