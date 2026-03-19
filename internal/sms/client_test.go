package sms

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSend_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/Messages.json" {
			t.Errorf("expected /Messages.json, got %s", r.URL.Path)
		}

		// Verify Basic Auth.
		user, pass, ok := r.BasicAuth()
		if !ok || user != "ACTEST" || pass != "token123" {
			t.Errorf("bad auth: user=%s pass=%s ok=%v", user, pass, ok)
		}

		_ = r.ParseForm()
		if r.FormValue("To") != "+1234567890" {
			t.Errorf("to = %s, want +1234567890", r.FormValue("To"))
		}
		if r.FormValue("From") != "+0987654321" {
			t.Errorf("from = %s, want +0987654321", r.FormValue("From"))
		}
		if r.FormValue("Body") != "Test message" {
			t.Errorf("body = %s, want 'Test message'", r.FormValue("Body"))
		}

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"sid":    "SM123",
			"status": "queued",
		})
	}))
	defer srv.Close()

	c := NewClient("ACTEST", "token123", "+0987654321")
	c.SetAPIURL(srv.URL)

	result, err := c.Send(context.Background(), "+1234567890", "Test message")
	if err != nil {
		t.Fatal(err)
	}
	if result.SID != "SM123" {
		t.Errorf("sid = %s, want SM123", result.SID)
	}
	if result.Status != "queued" {
		t.Errorf("status = %s, want queued", result.Status)
	}
}

func TestSend_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"Invalid To number"}`))
	}))
	defer srv.Close()

	c := NewClient("ACTEST", "token", "+1")
	c.SetAPIURL(srv.URL)

	_, err := c.Send(context.Background(), "bad", "test")
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
}

func TestSend_EmptyRecipient(t *testing.T) {
	c := NewClient("AC", "tok", "+1")
	_, err := c.Send(context.Background(), "", "body")
	if err == nil {
		t.Fatal("expected error for empty recipient")
	}
}

func TestSend_EmptyBody(t *testing.T) {
	c := NewClient("AC", "tok", "+1")
	_, err := c.Send(context.Background(), "+1", "")
	if err == nil {
		t.Fatal("expected error for empty body")
	}
}

func TestCheckStatus_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Messages/SM123.json" {
			t.Errorf("path = %s, want /Messages/SM123.json", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"sid":    "SM123",
			"status": "delivered",
		})
	}))
	defer srv.Close()

	c := NewClient("AC", "tok", "+1")
	c.SetAPIURL(srv.URL)

	result, err := c.CheckStatus(context.Background(), "SM123")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "delivered" {
		t.Errorf("status = %s, want delivered", result.Status)
	}
}
