package cloudloop

import (
	"encoding/json"
	"testing"
	"time"
)

func TestLingoTimestamp_Time(t *testing.T) {
	ts := LingoTimestamp{Year: 2026, Month: 3, Day: 23, Hour: 12, Minute: 30, Second: 45}
	got := ts.Time()
	want := time.Date(2026, 3, 23, 12, 30, 45, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("LingoTimestamp.Time() = %v, want %v", got, want)
	}
}

func TestLingoMO_ParseSBD(t *testing.T) {
	raw := `{
		"id": "abc-123-def",
		"receivedAt": {"year":2026,"month":3,"day":23,"hour":12,"minute":0,"second":0},
		"identity": {
			"accountId": "acct-001",
			"hardware": {"id":"hw-1","type":"ROCKBLOCK","imei":"300258060902280","serial":"1a07ty"},
			"thingId": "thing-001"
		},
		"sbd": {
			"imei": "300258060902280",
			"cdrReference": "cdr-ref-001",
			"momsn": 42,
			"mtmsn": 0,
			"sessionAt": {"year":2026,"month":3,"day":23,"hour":11,"minute":59,"second":50},
			"status": "SUCCESSFUL",
			"location": {"latitude":52.16,"longitude":4.51,"cep":10.0}
		},
		"message": "SGVsbG8gV29ybGQ="
	}`

	var mo LingoMO
	if err := json.Unmarshal([]byte(raw), &mo); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if mo.ID != "abc-123-def" {
		t.Errorf("ID = %q, want %q", mo.ID, "abc-123-def")
	}
	if imei := mo.ExtractIMEI(); imei != "300258060902280" {
		t.Errorf("ExtractIMEI() = %q, want %q", imei, "300258060902280")
	}
	if momsn := mo.MOMSN(); momsn != 42 {
		t.Errorf("MOMSN() = %d, want 42", momsn)
	}
	if src := mo.Source(); src != "cloudloop_sbd" {
		t.Errorf("Source() = %q, want %q", src, "cloudloop_sbd")
	}
	lat, lon, cep, ok := mo.Location()
	if !ok {
		t.Fatal("Location() returned ok=false")
	}
	if lat != 52.16 || lon != 4.51 || cep != 10.0 {
		t.Errorf("Location() = (%v, %v, %v), want (52.16, 4.51, 10.0)", lat, lon, cep)
	}
	tt := mo.TransmitTime()
	want := time.Date(2026, 3, 23, 11, 59, 50, 0, time.UTC)
	if !tt.Equal(want) {
		t.Errorf("TransmitTime() = %v, want %v", tt, want)
	}
}

func TestLingoMO_ParseIMT(t *testing.T) {
	raw := `{
		"id": "imt-456-ghi",
		"receivedAt": {"year":2026,"month":3,"day":23,"hour":14,"minute":0,"second":0},
		"identity": {
			"accountId": "acct-001",
			"hardware": {"id":"hw-2","type":"ROCKBLOCK_9704","imei":"300258060902281","serial":"rb9704"},
			"thingId": "thing-002"
		},
		"imt": {
			"cmid": "cm-001",
			"topic": "IMT_TOPIC_PURPLE",
			"messageId": 1,
			"crcError": false,
			"size": 256
		},
		"message": "dGVzdCBtZXNzYWdl"
	}`

	var mo LingoMO
	if err := json.Unmarshal([]byte(raw), &mo); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if mo.IMT == nil {
		t.Fatal("IMT is nil")
	}
	if mo.IMT.Topic != "IMT_TOPIC_PURPLE" {
		t.Errorf("IMT.Topic = %q, want %q", mo.IMT.Topic, "IMT_TOPIC_PURPLE")
	}
	if src := mo.Source(); src != "cloudloop_imt" {
		t.Errorf("Source() = %q, want %q", src, "cloudloop_imt")
	}
	if imei := mo.ExtractIMEI(); imei != "300258060902281" {
		t.Errorf("ExtractIMEI() = %q, want %q", imei, "300258060902281")
	}
	// IMT has no MOMSN
	if momsn := mo.MOMSN(); momsn != 0 {
		t.Errorf("MOMSN() = %d, want 0", momsn)
	}
	// IMT has no location
	_, _, _, ok := mo.Location()
	if ok {
		t.Error("Location() should return ok=false for IMT")
	}
}

func TestLingoMO_ParseCellular(t *testing.T) {
	raw := `{
		"id": "cell-789-jkl",
		"receivedAt": {"year":2026,"month":3,"day":23,"hour":15,"minute":30,"second":0},
		"identity": {
			"accountId": "acct-001",
			"thingId": "thing-003"
		},
		"cellular": {
			"mcn": "1234",
			"mcc": "204",
			"msisdn": "+31612345678",
			"imei": "353456789012345"
		},
		"message": "Y2VsbHVsYXIgdGVzdA=="
	}`

	var mo LingoMO
	if err := json.Unmarshal([]byte(raw), &mo); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if mo.Cellular == nil {
		t.Fatal("Cellular is nil")
	}
	if imei := mo.ExtractIMEI(); imei != "353456789012345" {
		t.Errorf("ExtractIMEI() = %q, want %q", imei, "353456789012345")
	}
	if src := mo.Source(); src != "cloudloop_cellular" {
		t.Errorf("Source() = %q, want %q", src, "cloudloop_cellular")
	}
}

func TestLingoMO_ExtractIMEI_Priority(t *testing.T) {
	// hardware.imei takes priority over sbd.imei
	raw := `{
		"id": "prio-test",
		"receivedAt": {"year":2026,"month":1,"day":1,"hour":0,"minute":0,"second":0},
		"identity": {
			"accountId": "acct",
			"hardware": {"id":"hw","type":"RB","imei":"HW_IMEI","serial":"s"},
			"thingId": "t"
		},
		"sbd": {
			"imei": "SBD_IMEI",
			"momsn": 1,
			"sessionAt": {"year":2026,"month":1,"day":1,"hour":0,"minute":0,"second":0},
			"status": "OK"
		},
		"message": ""
	}`

	var mo LingoMO
	if err := json.Unmarshal([]byte(raw), &mo); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if imei := mo.ExtractIMEI(); imei != "HW_IMEI" {
		t.Errorf("ExtractIMEI() = %q, want %q (hardware takes priority)", imei, "HW_IMEI")
	}
}

func TestLingoMO_NoIMEI(t *testing.T) {
	raw := `{
		"id": "no-imei",
		"receivedAt": {"year":2026,"month":1,"day":1,"hour":0,"minute":0,"second":0},
		"identity": {"accountId": "acct", "thingId": "t"},
		"message": ""
	}`

	var mo LingoMO
	if err := json.Unmarshal([]byte(raw), &mo); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if imei := mo.ExtractIMEI(); imei != "" {
		t.Errorf("ExtractIMEI() = %q, want empty", imei)
	}
}
