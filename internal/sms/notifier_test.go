package sms

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNotifier_SendsToPhoneNumbers(t *testing.T) {
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sent = append(sent, r.FormValue("To"))
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"sid": "SM1", "status": "queued"})
	}))
	defer srv.Close()

	c := NewClient("AC", "tok", "+1")
	c.SetAPIURL(srv.URL)
	n := NewNotifier(c)

	targets := []string{
		"+31612345678",         // phone — should send
		"slack://webhook/123",  // not phone — skip
		"+14155551234",         // phone — should send
		"mailto:test@test.com", // not phone — skip
	}

	err := n.Notify(context.Background(), targets, "SOS Alert", "Device dev1 triggered SOS")
	if err != nil {
		t.Fatal(err)
	}

	if len(sent) != 2 {
		t.Fatalf("expected 2 SMS sent, got %d", len(sent))
	}
	if sent[0] != "+31612345678" {
		t.Errorf("first = %s, want +31612345678", sent[0])
	}
	if sent[1] != "+14155551234" {
		t.Errorf("second = %s, want +14155551234", sent[1])
	}
}

func TestNotifier_SkipsNonPhoneTargets(t *testing.T) {
	c := NewClient("AC", "tok", "+1")
	n := NewNotifier(c)

	// All non-phone targets — should not attempt any sends.
	err := n.Notify(context.Background(), []string{
		"slack://token",
		"mailto:x@y.com",
		"https://webhook.example.com",
	}, "Test", "body")

	if err != nil {
		t.Fatal("unexpected error for non-phone targets")
	}
}

func TestFormatSMS_Short(t *testing.T) {
	text := formatSMS("SOS Alert", "Device dev1")
	if text != "SOS Alert | Device dev1" {
		t.Errorf("got %q", text)
	}
}

func TestFormatSMS_Truncated(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	text := formatSMS("Alert", long)
	if len(text) != 160 {
		t.Errorf("len = %d, want 160", len(text))
	}
	if text[157:] != "..." {
		t.Errorf("expected trailing ..., got %q", text[157:])
	}
}

func TestIsPhoneNumber(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"+31612345678", true},
		{"+12345", false},             // too short
		{"+12345678901234567", false}, // too long
		{"31612345678", false},        // no +
		{"slack://token", false},
		{"+14155551234", true},
	}
	for _, tt := range tests {
		if got := isPhoneNumber(tt.input); got != tt.want {
			t.Errorf("isPhoneNumber(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
