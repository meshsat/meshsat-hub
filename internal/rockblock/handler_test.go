package rockblock

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/compress"
)

const testSecret = "test-webhook-secret"

func TestHandler_NoSecret_Accepted(t *testing.T) {
	// Rock7 portal does not support webhook signing — no shared secret field.
	// When HUB_ROCKBLOCK_SECRET is empty, accept unsigned requests. [MESHSAT-446]
	h := &Handler{secret: ""}
	form := url.Values{"imei": {"300234065123456"}, "data": {"deadbeef"}}
	req := httptest.NewRequest("POST", "/api/webhook/rockblock",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 when secret not configured (Rock7 has no signing), got %d", w.Code)
	}
}

func TestHandler_ValidPayload_WithSecret(t *testing.T) {
	h := &Handler{secret: testSecret}

	plaintext := "Battery level 85 percent signal strength good"
	compressed := compress.CompressString(plaintext)
	dataHex := hex.EncodeToString(compressed)

	form := url.Values{
		"imei":              {"300234065123456"},
		"momsn":             {"42"},
		"transmit_time":     {"26-03-17 10:30:00"},
		"iridium_latitude":  {"52.1621"},
		"iridium_longitude": {"4.5094"},
		"iridium_cep":       {"10"},
		"data":              {dataHex},
		"JWT":               {testSecret},
	}

	req := httptest.NewRequest("POST", "/api/webhook/rockblock",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandler_MissingIMEI(t *testing.T) {
	h := &Handler{secret: testSecret}

	form := url.Values{
		"data": {"deadbeef"},
		"JWT":  {testSecret},
	}
	req := httptest.NewRequest("POST", "/api/webhook/rockblock",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandler_InvalidHex(t *testing.T) {
	h := &Handler{secret: testSecret}

	form := url.Values{
		"imei": {"300234065123456"},
		"data": {"not-valid-hex!!"},
		"JWT":  {testSecret},
	}
	req := httptest.NewRequest("POST", "/api/webhook/rockblock",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandler_SecretRequired_NoSecret(t *testing.T) {
	h := &Handler{secret: "mysecret"}

	form := url.Values{
		"imei": {"300234065123456"},
		"data": {"deadbeef"},
	}
	req := httptest.NewRequest("POST", "/api/webhook/rockblock",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestHandler_MethodNotAllowed(t *testing.T) {
	h := &Handler{secret: testSecret}

	req := httptest.NewRequest("GET", "/api/webhook/rockblock", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestIsPrintable(t *testing.T) {
	tests := []struct {
		input []byte
		want  bool
	}{
		{[]byte("hello world"), true},
		{[]byte("with\nnewline"), true},
		{[]byte{0x00, 0x01}, false},
		{[]byte{}, false},
		{[]byte("Battery 85%"), true},
	}
	for _, tt := range tests {
		got := isPrintable(tt.input)
		if got != tt.want {
			t.Errorf("isPrintable(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
