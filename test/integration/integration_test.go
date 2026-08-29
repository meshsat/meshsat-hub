//go:build integration

package integration

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/cloudloop"
	"github.com/meshsat/meshsat-hub/internal/compress"
	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
	"github.com/meshsat/meshsat-hub/internal/fragment"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
)

// --- MO Flow Tests (RockBLOCK Webhook → MQTT) ---

// TestMO_RockBLOCKWebhook_PublishesToMQTT posts a synthetic RockBLOCK webhook
// and verifies MO messages appear on the correct MQTT topics.
func TestMO_RockBLOCKWebhook_PublishesToMQTT(t *testing.T) {
	env := testStack(t)

	sub := testMQTTClient(t, env.BrokerAddr, "test-sub-mo")
	rawCollector := newCollector(t, sub, "meshsat/+/mo/raw")
	decodedCollector := newCollector(t, sub, "meshsat/+/mo/decoded")

	form := url.Values{
		"imei":              {"300234063904190"},
		"momsn":             {"42"},
		"transmit_time":     {"26-03-17 14:30:00"},
		"iridium_latitude":  {"52.3676"},
		"iridium_longitude": {"4.9041"},
		"iridium_cep":       {"8"},
		"data":              {hex.EncodeToString([]byte("Hello from space"))},
		"JWT":               {"test-secret"},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	env.Router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("webhook returned %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "ok" {
		t.Fatalf("expected status=ok, got %s", resp["status"])
	}

	// Verify mo/raw on MQTT.
	rawMsgs := rawCollector.wait(1, 3*time.Second)
	if len(rawMsgs) == 0 {
		t.Fatal("no mo/raw message received on MQTT")
	}
	if rawMsgs[0].Topic != "meshsat/300234063904190/mo/raw" {
		t.Errorf("mo/raw topic = %q, want meshsat/300234063904190/mo/raw", rawMsgs[0].Topic)
	}

	var rawPayload map[string]interface{}
	json.Unmarshal(rawMsgs[0].Payload, &rawPayload)
	if rawPayload["imei"] != "300234063904190" {
		t.Errorf("mo/raw imei = %v, want 300234063904190", rawPayload["imei"])
	}
	if rawPayload["momsn"].(float64) != 42 {
		t.Errorf("mo/raw momsn = %v, want 42", rawPayload["momsn"])
	}
	if rawPayload["channel"] != "iridium" {
		t.Errorf("mo/raw channel = %v, want iridium", rawPayload["channel"])
	}

	// Verify mo/decoded on MQTT.
	decodedMsgs := decodedCollector.wait(1, 3*time.Second)
	if len(decodedMsgs) == 0 {
		t.Fatal("no mo/decoded message received on MQTT")
	}

	var decoded map[string]interface{}
	json.Unmarshal(decodedMsgs[0].Payload, &decoded)
	if decoded["text"] != "Hello from space" {
		t.Errorf("mo/decoded text = %q, want 'Hello from space'", decoded["text"])
	}
	if decoded["iridium_latitude"].(float64) != 52.3676 {
		t.Errorf("iridium_latitude = %v, want 52.3676", decoded["iridium_latitude"])
	}
}

// TestMO_SMAZ2Compressed_DecodesCorrectly verifies SMAZ2 decompression end-to-end.
func TestMO_SMAZ2Compressed_DecodesCorrectly(t *testing.T) {
	env := testStack(t)

	sub := testMQTTClient(t, env.BrokerAddr, "test-sub-smaz2")
	decodedCollector := newCollector(t, sub, "meshsat/+/mo/decoded")

	original := "SOS emergency at base camp need immediate evacuation"
	compressed := compress.Compress([]byte(original))
	if len(compressed) == 0 {
		t.Fatal("SMAZ2 compression returned empty result")
	}
	t.Logf("SMAZ2: %d bytes → %d bytes (%.0f%%)", len(original), len(compressed),
		float64(len(compressed))/float64(len(original))*100)

	form := url.Values{
		"imei":              {"300234063904190"},
		"momsn":             {"43"},
		"transmit_time":     {"26-03-17 14:31:00"},
		"iridium_latitude":  {"0"},
		"iridium_longitude": {"0"},
		"iridium_cep":       {"0"},
		"data":              {hex.EncodeToString(compressed)},
		"JWT":               {"test-secret"},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	env.Router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("webhook returned %d: %s", w.Code, w.Body.String())
	}

	msgs := decodedCollector.wait(1, 3*time.Second)
	if len(msgs) == 0 {
		t.Fatal("no mo/decoded message received on MQTT")
	}

	var payload map[string]interface{}
	json.Unmarshal(msgs[0].Payload, &payload)

	if payload["text"] != original {
		t.Errorf("decompressed text = %q, want %q", payload["text"], original)
	}
	if payload["compressed"] != true {
		t.Errorf("compressed flag = %v, want true", payload["compressed"])
	}
	if payload["compression"] != "smaz2" {
		t.Errorf("compression = %v, want smaz2", payload["compression"])
	}
}

// TestMO_PositionPublished verifies position topic is published when lat/lon present.
func TestMO_PositionPublished(t *testing.T) {
	env := testStack(t)

	sub := testMQTTClient(t, env.BrokerAddr, "test-sub-pos")
	posCollector := newCollector(t, sub, "meshsat/+/position")

	form := url.Values{
		"imei":              {"300234063904190"},
		"momsn":             {"44"},
		"transmit_time":     {"26-03-17 15:00:00"},
		"iridium_latitude":  {"51.5074"},
		"iridium_longitude": {"-0.1278"},
		"iridium_cep":       {"5"},
		"data":              {hex.EncodeToString([]byte("test"))},
		"JWT":               {"test-secret"},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	env.Router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("webhook returned %d", w.Code)
	}

	posMsgs := posCollector.wait(1, 3*time.Second)
	if len(posMsgs) == 0 {
		t.Fatal("no position message received on MQTT")
	}

	if posMsgs[0].Topic != "meshsat/300234063904190/position" {
		t.Errorf("topic = %q, want meshsat/300234063904190/position", posMsgs[0].Topic)
	}

	var pos map[string]interface{}
	json.Unmarshal(posMsgs[0].Payload, &pos)
	if pos["lat"].(float64) != 51.5074 {
		t.Errorf("lat = %v, want 51.5074", pos["lat"])
	}
	if pos["source"] != "iridium_cep" {
		t.Errorf("source = %v, want iridium_cep", pos["source"])
	}
}

// --- MT Flow Tests (MQTT → Cloudloop API) ---

// TestMT_SendViaMQTT publishes an MT request to MQTT and verifies
// the Cloudloop API is called with the correct payload.
func TestMT_SendViaMQTT(t *testing.T) {
	env := testStack(t)

	sub := testMQTTClient(t, env.BrokerAddr, "test-sub-mt-status")
	statusCollector := newCollector(t, sub, "meshsat/+/mt/status")

	// Publish MT send request.
	mtReq := `{"text":"Reply from ground","compress":false}`
	imei := "300234063904190"
	topic := hubmqtt.TopicMTSend(imei)

	if err := env.HubMQTT.Publish(topic, 1, false, []byte(mtReq)); err != nil {
		t.Fatalf("publish MT request: %v", err)
	}

	// Wait for Cloudloop to be called.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		reqs := env.getCloudloopReqs()
		if len(reqs) > 0 {
			if reqs[0].IMEI != imei {
				t.Errorf("Cloudloop IMEI = %q, want %q", reqs[0].IMEI, imei)
			}
			// Verify payload is hex-encoded "Reply from ground"
			expected := hex.EncodeToString([]byte("Reply from ground"))
			if reqs[0].Data != expected {
				t.Errorf("Cloudloop data = %q, want %q", reqs[0].Data, expected)
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if len(env.getCloudloopReqs()) == 0 {
		t.Fatal("Cloudloop API was never called")
	}

	// Verify MT status published to MQTT.
	statusMsgs := statusCollector.wait(1, 5*time.Second)
	if len(statusMsgs) == 0 {
		t.Fatal("no mt/status message received on MQTT")
	}

	var status map[string]interface{}
	json.Unmarshal(statusMsgs[0].Payload, &status)
	if status["status"] != "queued" {
		t.Errorf("mt status = %v, want queued", status["status"])
	}
	if status["channel"] != "iridium" {
		t.Errorf("mt channel = %v, want iridium", status["channel"])
	}
}

// TestMT_CompressedSend verifies SMAZ2 compression is applied when requested.
func TestMT_CompressedSend(t *testing.T) {
	env := testStack(t)

	mtReq := `{"text":"emergency rescue at base camp","compress":true}`
	imei := "300234063904190"
	topic := hubmqtt.TopicMTSend(imei)

	if err := env.HubMQTT.Publish(topic, 1, false, []byte(mtReq)); err != nil {
		t.Fatalf("publish MT request: %v", err)
	}

	// Wait for Cloudloop call.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		reqs := env.getCloudloopReqs()
		if len(reqs) > 0 {
			// Decode the hex payload from Cloudloop.
			sentBytes, err := hex.DecodeString(reqs[0].Data)
			if err != nil {
				t.Fatalf("decode Cloudloop hex: %v", err)
			}
			// The compressed payload should be shorter than raw text.
			rawText := "emergency rescue at base camp"
			if len(sentBytes) >= len(rawText) {
				t.Errorf("compressed payload (%d bytes) not smaller than raw (%d bytes)",
					len(sentBytes), len(rawText))
			}
			// Decompress and verify round-trip.
			decompressed, err := compress.Decompress(sentBytes)
			if err != nil {
				t.Fatalf("decompress round-trip failed: %v", err)
			}
			if string(decompressed) != rawText {
				t.Errorf("round-trip text = %q, want %q", string(decompressed), rawText)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("Cloudloop API was never called")
}

// TestMT_CloudloopFailure verifies status="failed" when Cloudloop returns errors.
func TestMT_CloudloopFailure(t *testing.T) {
	env := testStack(t)

	// Replace Cloudloop mock with a failing one.
	env.CloudloopSrv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"server error"}`)
	})

	sub := testMQTTClient(t, env.BrokerAddr, "test-sub-mt-fail")
	statusCollector := newCollector(t, sub, "meshsat/+/mt/status")

	mtReq := `{"text":"this will fail"}`
	if err := env.HubMQTT.Publish(hubmqtt.TopicMTSend("300234063904190"), 1, false, []byte(mtReq)); err != nil {
		t.Fatalf("publish MT: %v", err)
	}

	// Wait for failed status (retries exhaust after ~7s: 1+2+4).
	statusMsgs := statusCollector.wait(1, 15*time.Second)
	if len(statusMsgs) == 0 {
		t.Fatal("no mt/status message received after Cloudloop failure")
	}

	var status map[string]interface{}
	json.Unmarshal(statusMsgs[0].Payload, &status)
	if status["status"] != "failed" {
		t.Errorf("mt status = %v, want failed", status["status"])
	}
	if status["error"] == nil || status["error"] == "" {
		t.Error("expected error message in failed status")
	}
}

// --- Webhook Auth Tests ---

// TestWebhookAuth_RejectsInvalidSecret verifies 401 on bad JWT secret.
func TestWebhookAuth_RejectsInvalidSecret(t *testing.T) {
	env := testStack(t)

	form := url.Values{
		"imei":  {"300234063904190"},
		"momsn": {"1"},
		"data":  {"48656c6c6f"},
		"JWT":   {"wrong-secret"},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	env.Router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

// TestWebhookAuth_RejectsMissingIMEI verifies 400 on missing IMEI.
func TestWebhookAuth_RejectsMissingIMEI(t *testing.T) {
	env := testStack(t)

	form := url.Values{
		"momsn": {"1"},
		"data":  {"48656c6c6f"},
		"JWT":   {"test-secret"},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	env.Router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// --- MQTT Message Format Validation ---

// TestMQTT_MODecodedFormat validates the JSON structure of mo/decoded messages.
func TestMQTT_MODecodedFormat(t *testing.T) {
	env := testStack(t)

	sub := testMQTTClient(t, env.BrokerAddr, "test-sub-format")
	decodedCollector := newCollector(t, sub, "meshsat/+/mo/decoded")

	form := url.Values{
		"imei":              {"300234063904190"},
		"momsn":             {"100"},
		"transmit_time":     {"26-03-17 16:00:00"},
		"iridium_latitude":  {"48.8566"},
		"iridium_longitude": {"2.3522"},
		"iridium_cep":       {"12"},
		"data":              {hex.EncodeToString([]byte("format check"))},
		"JWT":               {"test-secret"},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	env.Router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("webhook returned %d", w.Code)
	}

	msgs := decodedCollector.wait(1, 3*time.Second)
	if len(msgs) == 0 {
		t.Fatal("no mo/decoded message")
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(msgs[0].Payload, &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// Validate all required fields per CLAUDE.md spec.
	required := []string{"imei", "momsn", "channel", "text", "raw", "compressed", "encrypted", "transmit_time"}
	for _, f := range required {
		if _, ok := decoded[f]; !ok {
			t.Errorf("mo/decoded missing required field %q", f)
		}
	}

	if decoded["channel"] != "iridium" {
		t.Errorf("channel = %v, want iridium", decoded["channel"])
	}
	if decoded["encrypted"] != false {
		t.Errorf("encrypted = %v, want false", decoded["encrypted"])
	}
}

// --- E2E Encryption Tests ---

// TestMO_EncryptedPayload_DecryptsCorrectly verifies that an AES-256-GCM
// encrypted MO payload is decrypted when the device key is in the keystore.
func TestMO_EncryptedPayload_DecryptsCorrectly(t *testing.T) {
	env := testStack(t)

	sub := testMQTTClient(t, env.BrokerAddr, "test-sub-enc")
	decodedCollector := newCollector(t, sub, "meshsat/+/mo/decoded")

	imei := "300234063904190"
	original := "Encrypted SOS message from field"

	// Generate and register a key for this device.
	_, rawKey, err := env.KeyStore.GenerateAndStore(imei, "decrypt")
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	// Encrypt the message with the device key.
	encrypted, err := hubcrypto.Encrypt(rawKey, []byte(original))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	form := url.Values{
		"imei":              {imei},
		"momsn":             {"300"},
		"transmit_time":     {"26-03-17 20:00:00"},
		"iridium_latitude":  {"52.3676"},
		"iridium_longitude": {"4.9041"},
		"iridium_cep":       {"8"},
		"data":              {hex.EncodeToString(encrypted)},
		"JWT":               {"test-secret"},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	env.Router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("webhook returned %d: %s", w.Code, w.Body.String())
	}

	msgs := decodedCollector.wait(1, 3*time.Second)
	if len(msgs) == 0 {
		t.Fatal("no mo/decoded message received")
	}

	var decoded map[string]interface{}
	json.Unmarshal(msgs[0].Payload, &decoded)

	if decoded["text"] != original {
		t.Errorf("decrypted text = %q, want %q", decoded["text"], original)
	}
	if decoded["encrypted"] != true {
		t.Errorf("encrypted = %v, want true", decoded["encrypted"])
	}
}

// TestMO_EncryptedPayload_NoKey_PassesThrough verifies that an encrypted
// payload without a matching key passes through as raw bytes (backwards compatible).
func TestMO_EncryptedPayload_NoKey_PassesThrough(t *testing.T) {
	env := testStack(t)

	sub := testMQTTClient(t, env.BrokerAddr, "test-sub-enc-nokey")
	decodedCollector := newCollector(t, sub, "meshsat/+/mo/decoded")

	imei := "300234063904999" // different IMEI — no key registered

	// Create a fake "encrypted" payload (random bytes, won't decrypt).
	fakeEncrypted := make([]byte, hubcrypto.Overhead+20)
	for i := range fakeEncrypted {
		fakeEncrypted[i] = byte(i)
	}

	form := url.Values{
		"imei":              {imei},
		"momsn":             {"301"},
		"transmit_time":     {"26-03-17 20:01:00"},
		"iridium_latitude":  {"0"},
		"iridium_longitude": {"0"},
		"iridium_cep":       {"0"},
		"data":              {hex.EncodeToString(fakeEncrypted)},
		"JWT":               {"test-secret"},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	env.Router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("webhook returned %d: %s", w.Code, w.Body.String())
	}

	msgs := decodedCollector.wait(1, 3*time.Second)
	if len(msgs) == 0 {
		t.Fatal("no mo/decoded message received")
	}

	var decoded map[string]interface{}
	json.Unmarshal(msgs[0].Payload, &decoded)

	// Should pass through as non-encrypted (decryption silently failed).
	if decoded["encrypted"] != false {
		t.Errorf("encrypted = %v, want false (no key, should pass through)", decoded["encrypted"])
	}
}

// --- Fragment Reassembly Tests ---

// TestMO_FragmentReassembly_3Fragments sends a 3-fragment MO message via separate
// RockBLOCK webhooks and verifies the reassembled message appears on MQTT.
func TestMO_FragmentReassembly_3Fragments(t *testing.T) {
	env := testStack(t)

	sub := testMQTTClient(t, env.BrokerAddr, "test-sub-frag")
	decodedCollector := newCollector(t, sub, "meshsat/+/mo/decoded")

	imei := "300234063904190"
	original := "This is a long message that needs to be split across multiple SBD fragments for transmission over the Iridium satellite constellation. Each fragment carries a 2-byte header with the fragment index, total count, and message ID. The reassembler collects all fragments and combines them back into the original payload once all pieces arrive."

	// Fragment with a small MTU to force 3 fragments.
	// Each fragment = MTU bytes total (2 header + payload).
	// We need ceil(len(original) / (mtu-2)) == 3 fragments.
	payloadPerFrag := (len(original) + 2) / 3 // ensure 3 fragments
	mtu := payloadPerFrag + fragment.HeaderSize
	fragments := fragment.Fragment([]byte(original), mtu, 42)
	if len(fragments) != 3 {
		t.Fatalf("expected 3 fragments, got %d (mtu=%d, msgLen=%d)", len(fragments), mtu, len(original))
	}

	// Send each fragment as a separate webhook POST. First two should buffer.
	for i, frag := range fragments {
		form := url.Values{
			"imei":              {imei},
			"momsn":             {fmt.Sprintf("%d", 100+i)},
			"transmit_time":     {fmt.Sprintf("26-03-17 14:%02d:00", 30+i)},
			"iridium_latitude":  {"52.3676"},
			"iridium_longitude": {"4.9041"},
			"iridium_cep":       {"8"},
			"data":              {hex.EncodeToString(frag)},
			"JWT":               {"test-secret"},
		}

		req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		env.Router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("fragment %d: webhook returned %d: %s", i, w.Code, w.Body.String())
		}

		var resp map[string]string
		json.NewDecoder(w.Body).Decode(&resp)

		if i < 2 {
			// First two fragments should be buffered.
			if resp["status"] != "fragment_buffered" {
				t.Fatalf("fragment %d: expected status=fragment_buffered, got %s", i, resp["status"])
			}
		} else {
			// Last fragment triggers reassembly → full decoded message.
			if resp["status"] != "ok" {
				t.Fatalf("fragment %d: expected status=ok, got %s", i, resp["status"])
			}
		}
	}

	// Verify the reassembled message appears on mo/decoded.
	msgs := decodedCollector.wait(1, 3*time.Second)
	if len(msgs) == 0 {
		t.Fatal("no mo/decoded message received after fragment reassembly")
	}

	var decoded map[string]interface{}
	json.Unmarshal(msgs[0].Payload, &decoded)

	// The reassembled bytes go through SMAZ2 decompression attempt.
	// Since the original text is plain ASCII (not SMAZ2 compressed), the handler
	// falls back to raw text. Check we got the original message back.
	if decoded["text"] != original {
		t.Errorf("reassembled text = %q, want %q", decoded["text"], original)
	}
}

// --- SOS Detection Tests ---

// TestSOS_KeywordInMO_PublishesToSOSTopic sends a MO message containing "SOS"
// and verifies an SOS event appears on the sos MQTT topic.
func TestSOS_KeywordInMO_PublishesToSOSTopic(t *testing.T) {
	env := testStack(t)

	sub := testMQTTClient(t, env.BrokerAddr, "test-sub-sos")
	sosCollector := newCollector(t, sub, "meshsat/+/sos")

	form := url.Values{
		"imei":              {"300234063904190"},
		"momsn":             {"200"},
		"transmit_time":     {"26-03-17 18:00:00"},
		"iridium_latitude":  {"52.3676"},
		"iridium_longitude": {"4.9041"},
		"iridium_cep":       {"8"},
		"data":              {hex.EncodeToString([]byte("SOS need help at camp"))},
		"JWT":               {"test-secret"},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	env.Router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("webhook returned %d: %s", w.Code, w.Body.String())
	}

	sosMsgs := sosCollector.wait(1, 3*time.Second)
	if len(sosMsgs) == 0 {
		t.Fatal("no SOS event received on MQTT")
	}

	if sosMsgs[0].Topic != "meshsat/300234063904190/sos" {
		t.Errorf("SOS topic = %q, want meshsat/300234063904190/sos", sosMsgs[0].Topic)
	}

	var event map[string]interface{}
	json.Unmarshal(sosMsgs[0].Payload, &event)
	if event["imei"] != "300234063904190" {
		t.Errorf("SOS imei = %v, want 300234063904190", event["imei"])
	}
	if event["source"] != "keyword" {
		t.Errorf("SOS source = %v, want keyword", event["source"])
	}
	if event["keyword"] != "SOS" {
		t.Errorf("SOS keyword = %v, want SOS", event["keyword"])
	}
}

// TestSOS_NonSOSMessage_NoSOSEvent verifies that normal messages do not trigger SOS.
func TestSOS_NonSOSMessage_NoSOSEvent(t *testing.T) {
	env := testStack(t)

	sub := testMQTTClient(t, env.BrokerAddr, "test-sub-sos-neg")
	sosCollector := newCollector(t, sub, "meshsat/+/sos")

	form := url.Values{
		"imei":              {"300234063904190"},
		"momsn":             {"201"},
		"transmit_time":     {"26-03-17 18:01:00"},
		"iridium_latitude":  {"52.3676"},
		"iridium_longitude": {"4.9041"},
		"iridium_cep":       {"8"},
		"data":              {hex.EncodeToString([]byte("All clear at base camp"))},
		"JWT":               {"test-secret"},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	env.Router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("webhook returned %d: %s", w.Code, w.Body.String())
	}

	// Wait briefly — no SOS should arrive.
	sosMsgs := sosCollector.wait(1, 1*time.Second)
	if len(sosMsgs) > 0 {
		t.Errorf("unexpected SOS event for non-SOS message: %s", string(sosMsgs[0].Payload))
	}
}

// TestSOS_MAYDAYKeyword verifies MAYDAY keyword triggers SOS.
func TestSOS_MAYDAYKeyword(t *testing.T) {
	env := testStack(t)

	sub := testMQTTClient(t, env.BrokerAddr, "test-sub-sos-mayday")
	sosCollector := newCollector(t, sub, "meshsat/+/sos")

	form := url.Values{
		"imei":              {"300234063904190"},
		"momsn":             {"202"},
		"transmit_time":     {"26-03-17 18:02:00"},
		"iridium_latitude":  {"0"},
		"iridium_longitude": {"0"},
		"iridium_cep":       {"0"},
		"data":              {hex.EncodeToString([]byte("mayday mayday vessel sinking"))},
		"JWT":               {"test-secret"},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	env.Router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("webhook returned %d: %s", w.Code, w.Body.String())
	}

	sosMsgs := sosCollector.wait(1, 3*time.Second)
	if len(sosMsgs) == 0 {
		t.Fatal("no SOS event for MAYDAY message")
	}

	var event map[string]interface{}
	json.Unmarshal(sosMsgs[0].Payload, &event)
	if event["keyword"] != "MAYDAY" {
		t.Errorf("SOS keyword = %v, want MAYDAY", event["keyword"])
	}
}

// --- SMAZ2 Round-Trip ---

// --- Cloudloop Webhook Tests (LingoMO — SBD + IMT) ---

// TestCloudloop_SBD_MO_PublishesToMQTT posts a synthetic Cloudloop SBD LingoMO
// and verifies MO messages appear on the correct MQTT topics.
func TestCloudloop_SBD_MO_PublishesToMQTT(t *testing.T) {
	env := testStack(t)

	sub := testMQTTClient(t, env.BrokerAddr, "test-sub-cl-sbd")
	rawCollector := newCollector(t, sub, "meshsat/+/mo/raw")
	decodedCollector := newCollector(t, sub, "meshsat/+/mo/decoded")
	posCollector := newCollector(t, sub, "meshsat/+/position")

	mo := cloudloop.LingoMO{
		ID:         "cl-sbd-001",
		ReceivedAt: cloudloop.LingoTimestamp{Year: 2026, Month: 3, Day: 27, Hour: 12},
		Identity: cloudloop.LingoIdentity{
			AccountID: "acct-e2e",
			Hardware:  &cloudloop.LingoHardware{ID: "hw-1", Type: "ROCKBLOCK_9603", IMEI: "300234063904190", Serial: "rb9603"},
			ThingID:   "thing-sbd-e2e",
		},
		SBD: &cloudloop.LingoSBD{
			IMEI:      "300234063904190",
			MOMSN:     55,
			SessionAt: cloudloop.LingoTimestamp{Year: 2026, Month: 3, Day: 27, Hour: 12},
			Status:    "OK",
			Location:  &cloudloop.LingoLocation{Latitude: 52.37, Longitude: 4.90, CEP: 8.0},
		},
		Message: "SGVsbG8gZnJvbSBTQkQ=", // "Hello from SBD"
	}
	body, _ := json.Marshal(mo)

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/cloudloop", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.Router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("webhook returned %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "ok" {
		t.Fatalf("expected status=ok, got %s", resp["status"])
	}

	// Verify mo/raw.
	rawMsgs := rawCollector.wait(1, 3*time.Second)
	if len(rawMsgs) == 0 {
		t.Fatal("no mo/raw message received on MQTT")
	}
	if rawMsgs[0].Topic != "meshsat/300234063904190/mo/raw" {
		t.Errorf("mo/raw topic = %q, want meshsat/300234063904190/mo/raw", rawMsgs[0].Topic)
	}

	// Verify mo/decoded.
	decodedMsgs := decodedCollector.wait(1, 3*time.Second)
	if len(decodedMsgs) == 0 {
		t.Fatal("no mo/decoded message received on MQTT")
	}
	var decoded map[string]interface{}
	json.Unmarshal(decodedMsgs[0].Payload, &decoded)
	if decoded["text"] != "Hello from SBD" {
		t.Errorf("text = %q, want 'Hello from SBD'", decoded["text"])
	}
	if decoded["source"] != "cloudloop_sbd" {
		t.Errorf("source = %q, want cloudloop_sbd", decoded["source"])
	}

	// Verify position (SBD has Iridium CEP location).
	posMsgs := posCollector.wait(1, 3*time.Second)
	if len(posMsgs) == 0 {
		t.Fatal("no position message received on MQTT for SBD")
	}
	var pos map[string]interface{}
	json.Unmarshal(posMsgs[0].Payload, &pos)
	if pos["lat"].(float64) != 52.37 {
		t.Errorf("lat = %v, want 52.37", pos["lat"])
	}
}

// TestCloudloop_IMT_MO_PublishesToMQTT posts a synthetic Cloudloop IMT LingoMO
// and verifies MO messages appear on the correct MQTT topics.
func TestCloudloop_IMT_MO_PublishesToMQTT(t *testing.T) {
	env := testStack(t)

	sub := testMQTTClient(t, env.BrokerAddr, "test-sub-cl-imt")
	decodedCollector := newCollector(t, sub, "meshsat/+/mo/decoded")

	mo := cloudloop.LingoMO{
		ID:         "cl-imt-001",
		ReceivedAt: cloudloop.LingoTimestamp{Year: 2026, Month: 3, Day: 27, Hour: 13},
		Identity: cloudloop.LingoIdentity{
			AccountID: "acct-e2e",
			Hardware:  &cloudloop.LingoHardware{ID: "hw-2", Type: "ROCKBLOCK_9704", IMEI: "300258060902280", Serial: "rb9704"},
			ThingID:   "thing-imt-e2e",
		},
		IMT: &cloudloop.LingoIMT{
			CMID:      "cm-e2e",
			Topic:     "IMT_TOPIC_PURPLE",
			MessageID: "msg-imt-e2e",
			Size:      22,
		},
		Message: "SGVsbG8gZnJvbSBJTVQgOTcwNA==", // "Hello from IMT 9704"
	}
	body, _ := json.Marshal(mo)

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/cloudloop", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.Router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("webhook returned %d: %s", w.Code, w.Body.String())
	}

	// Verify mo/decoded.
	decodedMsgs := decodedCollector.wait(1, 3*time.Second)
	if len(decodedMsgs) == 0 {
		t.Fatal("no mo/decoded message received on MQTT")
	}
	var decoded map[string]interface{}
	json.Unmarshal(decodedMsgs[0].Payload, &decoded)
	if decoded["text"] != "Hello from IMT 9704" {
		t.Errorf("text = %q, want 'Hello from IMT 9704'", decoded["text"])
	}
	if decoded["source"] != "cloudloop_imt" {
		t.Errorf("source = %q, want cloudloop_imt", decoded["source"])
	}
	if decoded["imei"] != "300258060902280" {
		t.Errorf("imei = %q, want 300258060902280", decoded["imei"])
	}
}

// TestCloudloop_DualProtocol_SBDandIMT sends both SBD and IMT MO messages
// through the Cloudloop webhook and verifies both paths work independently.
func TestCloudloop_DualProtocol_SBDandIMT(t *testing.T) {
	env := testStack(t)

	sub := testMQTTClient(t, env.BrokerAddr, "test-sub-cl-dual")
	decodedCollector := newCollector(t, sub, "meshsat/+/mo/decoded")

	// SBD MO (9603).
	sbdMO := cloudloop.LingoMO{
		ID:         "cl-dual-sbd",
		ReceivedAt: cloudloop.LingoTimestamp{Year: 2026, Month: 3, Day: 27, Hour: 14},
		Identity: cloudloop.LingoIdentity{
			AccountID: "acct",
			Hardware:  &cloudloop.LingoHardware{IMEI: "300234063904190"},
			ThingID:   "t-sbd",
		},
		SBD: &cloudloop.LingoSBD{
			IMEI:      "300234063904190",
			MOMSN:     60,
			SessionAt: cloudloop.LingoTimestamp{Year: 2026, Month: 3, Day: 27, Hour: 14},
			Status:    "OK",
		},
		Message: "U0JEIHBhdGg=", // "SBD path"
	}

	// IMT MO (9704).
	imtMO := cloudloop.LingoMO{
		ID:         "cl-dual-imt",
		ReceivedAt: cloudloop.LingoTimestamp{Year: 2026, Month: 3, Day: 27, Hour: 14, Minute: 1},
		Identity: cloudloop.LingoIdentity{
			AccountID: "acct",
			Hardware:  &cloudloop.LingoHardware{IMEI: "300258060902280"},
			ThingID:   "t-imt",
		},
		IMT: &cloudloop.LingoIMT{
			CMID:      "cm",
			Topic:     "IMT_TOPIC_PURPLE",
			MessageID: "msg-dual-imt",
			Size:      8,
		},
		Message: "SU1UIHBhdGg=", // "IMT path"
	}

	for _, mo := range []cloudloop.LingoMO{sbdMO, imtMO} {
		body, _ := json.Marshal(mo)
		req := httptest.NewRequest(http.MethodPost, "/api/webhook/cloudloop", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		env.Router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("webhook returned %d for %s: %s", w.Code, mo.ID, w.Body.String())
		}
	}

	// Collect both decoded messages.
	msgs := decodedCollector.wait(2, 5*time.Second)
	if len(msgs) < 2 {
		t.Fatalf("expected 2 decoded messages, got %d", len(msgs))
	}

	sources := make(map[string]string) // source -> text
	for _, m := range msgs {
		var d map[string]interface{}
		json.Unmarshal(m.Payload, &d)
		if src, ok := d["source"].(string); ok {
			sources[src] = fmt.Sprintf("%v", d["text"])
		}
	}

	if sources["cloudloop_sbd"] != "SBD path" {
		t.Errorf("SBD text = %q, want 'SBD path'", sources["cloudloop_sbd"])
	}
	if sources["cloudloop_imt"] != "IMT path" {
		t.Errorf("IMT text = %q, want 'IMT path'", sources["cloudloop_imt"])
	}
}

// TestSMAZ2_RoundTrip validates SMAZ2 compress/decompress across various inputs.
func TestSMAZ2_RoundTrip(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"short", "hello"},
		{"meshtastic terms", "emergency rescue at base camp position report"},
		{"mixed", "Node 42 at latitude 52.3 signal 3 bars battery 78 percent"},
		{"long", strings.Repeat("satellite iridium modem ", 10)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compressed := compress.Compress([]byte(tt.text))
			if len(compressed) == 0 {
				t.Fatal("compression returned empty")
			}

			decompressed, err := compress.Decompress(compressed)
			if err != nil {
				t.Fatalf("decompression failed: %v", err)
			}
			if string(decompressed) != tt.text {
				t.Errorf("round-trip mismatch: got %q, want %q", string(decompressed), tt.text)
			}
		})
	}
}
