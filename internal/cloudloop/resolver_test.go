package cloudloop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestThingResolver_ResolveUnknown(t *testing.T) {
	r := NewThingResolver(nil)

	thingID, isIMT := r.Resolve("300234065000001")
	if thingID != "300234065000001" {
		t.Errorf("expected IMEI as thingID for unknown device, got %s", thingID)
	}
	if isIMT {
		t.Error("expected isIMT=false for unknown device")
	}
}

func TestThingResolver_Register(t *testing.T) {
	r := NewThingResolver(nil)

	r.Register("300234065000001", "AbCdEfGh12345678901234567890AB", false)
	thingID, isIMT := r.Resolve("300234065000001")
	if thingID != "AbCdEfGh12345678901234567890AB" {
		t.Errorf("expected registered thingID, got %s", thingID)
	}
	if isIMT {
		t.Error("expected isIMT=false for SBD device")
	}

	r.Register("300258060902280", "XyZ9704ThingId00000000000000AB", true)
	thingID, isIMT = r.Resolve("300258060902280")
	if thingID != "XyZ9704ThingId00000000000000AB" {
		t.Errorf("expected registered thingID, got %s", thingID)
	}
	if !isIMT {
		t.Error("expected isIMT=true for IMT device")
	}
}

func TestThingResolver_Count(t *testing.T) {
	r := NewThingResolver(nil)
	if r.Count() != 0 {
		t.Errorf("expected 0, got %d", r.Count())
	}

	r.Register("300234065000001", "thing1", false)
	r.Register("300234065000002", "thing2", true)
	if r.Count() != 2 {
		t.Errorf("expected 2, got %d", r.Count())
	}
}

func TestThingResolver_LearnFromMO_SBD(t *testing.T) {
	r := NewThingResolver(nil)

	mo := &LingoMO{
		ID: "mo-001",
		Identity: LingoIdentity{
			ThingID: "SbdThingId123456789012345678AB",
			Hardware: &LingoHardware{
				IMEI: "300234065000001",
				Type: "HARDWARE_TYPE_IRIDIUM_SBD",
			},
		},
		SBD: &LingoSBD{
			IMEI:  "300234065000001",
			MOMSN: 42,
		},
	}

	r.LearnFromMO(mo)

	thingID, isIMT := r.Resolve("300234065000001")
	if thingID != "SbdThingId123456789012345678AB" {
		t.Errorf("expected learned thingID, got %s", thingID)
	}
	if isIMT {
		t.Error("expected isIMT=false for SBD device")
	}
}

func TestThingResolver_LearnFromMO_IMT(t *testing.T) {
	r := NewThingResolver(nil)

	mo := &LingoMO{
		ID: "mo-002",
		Identity: LingoIdentity{
			ThingID: "ImtThingId123456789012345678AB",
			Hardware: &LingoHardware{
				IMEI: "300258060902280",
				Type: "HARDWARE_TYPE_IRIDIUM_CERTUS",
			},
		},
		IMT: &LingoIMT{
			CMID:  "cmid-001",
			Topic: "IMT_TOPIC_PURPLE",
		},
	}

	r.LearnFromMO(mo)

	thingID, isIMT := r.Resolve("300258060902280")
	if thingID != "ImtThingId123456789012345678AB" {
		t.Errorf("expected learned thingID, got %s", thingID)
	}
	if !isIMT {
		t.Error("expected isIMT=true for IMT device")
	}
}

func TestThingResolver_LearnFromMO_IMTByHardwareType(t *testing.T) {
	r := NewThingResolver(nil)

	// IMT detected by hardware type string, even without IMT field.
	mo := &LingoMO{
		ID: "mo-003",
		Identity: LingoIdentity{
			ThingID: "CertusThingABCDEFGH1234567890",
			Hardware: &LingoHardware{
				IMEI: "300258060000001",
				Type: "HARDWARE_TYPE_IRIDIUM_CERTUS",
			},
		},
		SBD: &LingoSBD{
			IMEI: "300258060000001",
		},
	}

	r.LearnFromMO(mo)

	_, isIMT := r.Resolve("300258060000001")
	if !isIMT {
		t.Error("expected isIMT=true when hardware type contains CERTUS")
	}
}

func TestThingResolver_LearnFromMO_IMTBySubscriberType(t *testing.T) {
	r := NewThingResolver(nil)

	mo := &LingoMO{
		ID: "mo-004",
		Identity: LingoIdentity{
			ThingID: "P6ThingABCDEFGH12345678901234",
			Subscriber: &LingoSubscriber{
				ID:   "sub-001",
				Type: "SUBSCRIBER_TYPE_P6",
			},
			Hardware: &LingoHardware{
				IMEI: "300258060000002",
				Type: "HARDWARE_TYPE_UNKNOWN",
			},
		},
	}

	r.LearnFromMO(mo)

	_, isIMT := r.Resolve("300258060000002")
	if !isIMT {
		t.Error("expected isIMT=true when subscriber type contains P6")
	}
}

func TestThingResolver_LearnFromMO_NilSafe(t *testing.T) {
	r := NewThingResolver(nil)

	// nil MO should not panic.
	r.LearnFromMO(nil)

	// MO with no IMEI should not cache.
	r.LearnFromMO(&LingoMO{
		ID:       "mo-005",
		Identity: LingoIdentity{ThingID: "thing123"},
	})
	if r.Count() != 0 {
		t.Errorf("expected 0 cached entries for MO without IMEI, got %d", r.Count())
	}

	// MO with no thingID should not cache.
	r.LearnFromMO(&LingoMO{
		ID: "mo-006",
		Identity: LingoIdentity{
			Hardware: &LingoHardware{IMEI: "300234065000001"},
		},
	})
	if r.Count() != 0 {
		t.Errorf("expected 0 cached entries for MO without thingID, got %d", r.Count())
	}
}

func TestThingResolver_LearnFromMO_OverwritesPrevious(t *testing.T) {
	r := NewThingResolver(nil)

	// First MO: device is SBD.
	mo1 := &LingoMO{
		ID: "mo-010",
		Identity: LingoIdentity{
			ThingID:  "OldThingId1234567890123456789A",
			Hardware: &LingoHardware{IMEI: "300234065000001", Type: "HARDWARE_TYPE_IRIDIUM_SBD"},
		},
	}
	r.LearnFromMO(mo1)

	thingID, _ := r.Resolve("300234065000001")
	if thingID != "OldThingId1234567890123456789A" {
		t.Errorf("expected old thingID, got %s", thingID)
	}

	// Second MO: same IMEI, new thingID (device was re-provisioned).
	mo2 := &LingoMO{
		ID: "mo-011",
		Identity: LingoIdentity{
			ThingID:  "NewThingId1234567890123456789B",
			Hardware: &LingoHardware{IMEI: "300234065000001", Type: "HARDWARE_TYPE_IRIDIUM_SBD"},
		},
	}
	r.LearnFromMO(mo2)

	thingID, _ = r.Resolve("300234065000001")
	if thingID != "NewThingId1234567890123456789B" {
		t.Errorf("expected new thingID after re-learn, got %s", thingID)
	}
}

func TestListThings_Success(t *testing.T) {
	things := []CloudloopThing{
		{
			ID:          "ThingABCDEFGH123456789012345A",
			SupportsSBD: true,
			SubscriberSBD: &CloudloopSubscRef{
				ID:   "sub-sbd-001",
				IMEI: "300234065000001",
			},
		},
		{
			ID:         "ThingXYZ98765432109876543210BC",
			SupportsP6: true,
			SubscriberCertus: &CloudloopSubscRef{
				ID:   "sub-certus-001",
				IMEI: "300258060902280",
			},
		},
		{
			ID:          "ThingNoIMEI000000000000000000",
			SupportsSBD: true,
			// No subscriber refs with IMEI.
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/Data/GetThings" {
			t.Errorf("expected /Data/GetThings, got %s", r.URL.Path)
		}
		token := r.URL.Query().Get("token")
		if token != "test-key" {
			t.Errorf("wrong token: %s", token)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GetThingsResponse{Things: things})
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-key")
	result, err := client.ListThings(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 things, got %d", len(result))
	}
	if result[0].ID != "ThingABCDEFGH123456789012345A" {
		t.Errorf("wrong first thing ID: %s", result[0].ID)
	}
	if !result[1].SupportsP6 {
		t.Error("expected second thing to support P6")
	}
}

func TestListThings_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"invalid token"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "bad-key")
	_, err := client.ListThings(context.Background())
	if err == nil {
		t.Error("expected error for 403 response")
	}
}

func TestThingResolver_RefreshFromAPI(t *testing.T) {
	things := []CloudloopThing{
		{
			ID:          "ThingSBD00000000000000000000AB",
			SupportsSBD: true,
			SubscriberSBD: &CloudloopSubscRef{
				ID:   "sub-sbd-001",
				IMEI: "300234065000001",
			},
		},
		{
			ID:         "ThingIMT00000000000000000000CD",
			SupportsP6: true,
			SubscriberCertus: &CloudloopSubscRef{
				ID:   "sub-certus-001",
				IMEI: "300258060902280",
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GetThingsResponse{Things: things})
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-key")
	r := NewThingResolver(client)

	if err := r.RefreshFromAPI(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if r.Count() != 2 {
		t.Fatalf("expected 2 cached entries, got %d", r.Count())
	}

	// SBD device.
	thingID, isIMT := r.Resolve("300234065000001")
	if thingID != "ThingSBD00000000000000000000AB" {
		t.Errorf("expected SBD thingID, got %s", thingID)
	}
	if isIMT {
		t.Error("expected isIMT=false for SBD device")
	}

	// IMT/Certus device.
	thingID, isIMT = r.Resolve("300258060902280")
	if thingID != "ThingIMT00000000000000000000CD" {
		t.Errorf("expected IMT thingID, got %s", thingID)
	}
	if !isIMT {
		t.Error("expected isIMT=true for Certus device")
	}
}

func TestThingResolver_RefreshFromAPI_NoClient(t *testing.T) {
	r := NewThingResolver(nil)
	err := r.RefreshFromAPI(context.Background())
	if err == nil {
		t.Error("expected error when no client configured")
	}
}

func TestThingResolver_RefreshFromAPI_SkipsExisting(t *testing.T) {
	things := []CloudloopThing{
		{
			ID:          "APIThingId00000000000000000001",
			SupportsSBD: true,
			SubscriberSBD: &CloudloopSubscRef{
				ID:   "sub-001",
				IMEI: "300234065000001",
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GetThingsResponse{Things: things})
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-key")
	r := NewThingResolver(client)

	// Pre-register via MO (simulating a learned mapping).
	r.Register("300234065000001", "MOLearnedThingId0000000000001A", true)

	if err := r.RefreshFromAPI(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should NOT overwrite the MO-learned mapping.
	thingID, isIMT := r.Resolve("300234065000001")
	if thingID != "MOLearnedThingId0000000000001A" {
		t.Errorf("expected MO-learned thingID to be preserved, got %s", thingID)
	}
	if !isIMT {
		t.Error("expected MO-learned isIMT to be preserved")
	}
}

func TestThingResolver_ImplementsDeviceResolver(t *testing.T) {
	var _ DeviceResolver = (*ThingResolver)(nil)
}

// IFRNLLEI01PRD-906: Cloudloop changed subscriberCertus from object to string,
// which used to fail the entire GetThings decode once per minute. Verify both
// shapes (and null) now parse.
func TestCloudloopSubscRef_UnmarshalJSON_StringOrObject(t *testing.T) {
	var obj CloudloopSubscRef
	if err := json.Unmarshal([]byte(`{"id":"sub-1","imei":"300123"}`), &obj); err != nil {
		t.Fatalf("object form: unexpected error: %v", err)
	}
	if obj.ID != "sub-1" || obj.IMEI != "300123" {
		t.Fatalf("object form: got %+v", obj)
	}
	var str CloudloopSubscRef
	if err := json.Unmarshal([]byte(`"sub-2"`), &str); err != nil {
		t.Fatalf("string form: unexpected error: %v", err)
	}
	if str.ID != "sub-2" {
		t.Fatalf("string form: got %+v", str)
	}
	var nul CloudloopSubscRef
	if err := json.Unmarshal([]byte(`null`), &nul); err != nil {
		t.Fatalf("null: unexpected error: %v", err)
	}
	if nul.ID != "" {
		t.Fatalf("null: expected zero value, got %+v", nul)
	}
	// The exact -906 failure mode: a full GetThings response with a STRING
	// subscriberCertus must decode cleanly (object subscriberSbd alongside).
	body := `{"things":[{"id":"t1","account":"a","supportsP6":true,"subscriberCertus":"certus-ref-123","subscriberSbd":{"id":"sbd-1","imei":"300999"}}]}`
	var resp GetThingsResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("GetThings with string subscriberCertus: unexpected error: %v", err)
	}
	if len(resp.Things) != 1 || resp.Things[0].SubscriberCertus == nil || resp.Things[0].SubscriberCertus.ID != "certus-ref-123" {
		t.Fatalf("GetThings: subscriberCertus not parsed: %+v", resp.Things)
	}
	if resp.Things[0].SubscriberSBD == nil || resp.Things[0].SubscriberSBD.IMEI != "300999" {
		t.Fatalf("GetThings: subscriberSbd object not parsed: %+v", resp.Things[0].SubscriberSBD)
	}
}
