package sms

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Every outbound message must carry a StatusCallback, or a carrier drop is
// invisible.
//
// This is the gap behind both "nothing happened" reports at the stand: Twilio
// accepts the send, answers "queued", and the Hub logs a success for a message
// nobody received (MESHSAT-1175). A phone number has no SMS status field -- its
// status_callback is the VOICE one -- so the only place this can be set is per
// message, here.
func TestEveryOutboundMessageAsksForADeliveryReceipt(t *testing.T) {
	SetPublicBaseURL("https://hub.meshsat.net")
	defer SetPublicBaseURL("")

	for _, tc := range []struct {
		name, channel, want string
	}{
		{"sms", "", "https://hub.meshsat.net/api/webhook/sms/status"},
		{"sms explicit", "sms", "https://hub.meshsat.net/api/webhook/sms/status"},
		{"whatsapp", "whatsapp", "https://hub.meshsat.net/api/webhook/whatsapp/status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				got = r.FormValue("StatusCallback")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"sid":"SM1","status":"queued"}`))
			}))
			defer srv.Close()

			c := NewClient("ACTEST", "token", "+3197000000001")
			c.SetAPIURL(srv.URL)
			c.SetChannel(tc.channel)

			if _, err := c.Send(context.Background(), "+31600000001", "hello"); err != nil {
				t.Fatalf("send: %v", err)
			}
			if got != tc.want {
				t.Errorf("StatusCallback = %q, want %q -- without it a dropped message looks delivered", got, tc.want)
			}

			// A Content template is the booth's interactive path and is billed
			// and dropped exactly the same way, so it must ask too.
			got = ""
			if _, err := c.SendContent(context.Background(), "+31600000001", "HX123", ""); err != nil {
				t.Fatalf("send content: %v", err)
			}
			if got != tc.want {
				t.Errorf("SendContent StatusCallback = %q, want %q", got, tc.want)
			}
		})
	}
}

// With no public origin configured there is nowhere to send a receipt, and a
// relative or empty callback would make Twilio reject the whole send. Losing
// receipts is survivable; losing the message is not.
func TestNoPublicURLMeansNoCallbackRatherThanABrokenSend(t *testing.T) {
	SetPublicBaseURL("")

	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		_, present = r.Form["StatusCallback"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sid":"SM1","status":"queued"}`))
	}))
	defer srv.Close()

	c := NewClient("ACTEST", "token", "+3197000000001")
	c.SetAPIURL(srv.URL)
	if _, err := c.Send(context.Background(), "+31600000001", "hello"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if present {
		t.Error("sent an empty StatusCallback; Twilio rejects that and the message would never go out")
	}
}
