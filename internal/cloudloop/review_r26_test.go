package cloudloop

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/compress"
)

func TestReviewR26VersionedSmaz2RemainsReadable(t *testing.T) {
	const imei = "300000000000091"
	const plain = "Team at the north gate, all safe, moving to camp"
	packed := compress.Compress([]byte(plain))
	back, err := compress.Decompress(packed)
	if err != nil || string(back) != plain {
		t.Fatalf("invalid fixture: %q %v", back, err)
	}
	wire := append([]byte{0x01}, packed...)
	b := &ackBus{}
	h := NewWebhookHandler(b)
	mo := rnsMO(imei, IMTTopicRaw, wire, "00000000-0000-4000-8000-000000000091")
	h.ProcessLingoMO(context.Background(), mo)
	b.mu.Lock()
	published := append([]byte(nil), b.msgs["meshsat/"+imei+"/mo/decoded"]...)
	b.mu.Unlock()
	var got WebhookMOMessage
	if err := json.Unmarshal(published, &got); err != nil {
		t.Fatal(err)
	}
	t.Logf("plain=%d compressed=%d wire=%d opaque=%v text=%q compressed_flag=%v wire_preserved=%v", len(plain), len(packed), len(wire), got.Opaque, got.Text, got.Compressed, got.Wire == base64.StdEncoding.EncodeToString(wire))
	if got.Opaque || got.Text != plain || !got.Compressed {
		t.Fatal("valid unencrypted versioned SMAZ2 was hidden as ciphertext")
	}
}
