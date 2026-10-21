package cloudloop

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/codec"
	"github.com/meshsat/meshsat-hub/internal/compress"
	"github.com/meshsat/meshsat-hub/internal/store"
)

type reviewR27DeclarationStore struct {
	birth string
	err   error
}

func (s *reviewR27DeclarationStore) DeviceBridgeID(context.Context, string, string) (string, error) {
	return "review-kit", nil
}

func (s *reviewR27DeclarationStore) GetBridge(context.Context, string, string) (*store.Bridge, error) {
	return &store.Bridge{BridgeID: "review-kit", LastBirth: s.birth}, s.err
}

func reviewR27Ciphertext(t *testing.T) []byte {
	t.Helper()
	sealed, err := hex.DecodeString("42f28a46039f894d3a0179d07a753f51ccddb2686d236a400d8ee239f8d1")
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], nil)
	if err != nil || string(opened) != string([]byte{0, 'k'}) {
		t.Fatalf("invalid AES-GCM fixture: %v", err)
	}
	decoded, err := compress.Decompress(sealed)
	if err != nil || len(decoded) == 0 || !isPrintable(decoded) {
		t.Fatal("fixture does not trigger the legacy SMAZ2 guess")
	}
	return append([]byte{codec.ProtoVersion1}, sealed...)
}

func TestReviewR27KnownCiphertextStaysOpaque(t *testing.T) {
	const imei = "300000000000091"
	wire := reviewR27Ciphertext(t)
	fullBirth := fmt.Sprintf(`{"bridge_id":"review-kit","interfaces":[{"name":"iridium_imt","imei":%q,"egress_sealed":true}]}`, imei)
	for _, scenario := range []string{"late_imei", "gateway_absent", "lookup_error"} {
		t.Run(scenario, func(t *testing.T) {
			s := &reviewR27DeclarationStore{birth: fullBirth}
			b := &ackBus{}
			h := NewWebhookHandler(b)
			h.SetEgressDeclarations(bridge.NewEgressDeclarations(s))
			process := func(seq int) WebhookMOMessage {
				mo := rnsMO(imei, IMTTopicRaw, wire, fmt.Sprintf("00000000-0000-4000-8000-%012d", seq))
				h.ProcessLingoMO(context.Background(), mo)
				b.mu.Lock()
				data := append([]byte(nil), b.msgs["meshsat/"+imei+"/mo/decoded"]...)
				b.mu.Unlock()
				var got WebhookMOMessage
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatal(err)
				}
				return got
			}
			before := process(1)
			if !before.Opaque || before.Text != "" {
				t.Fatalf("declared control failed: %+v", before)
			}
			switch scenario {
			case "late_imei":
				s.birth = `{"bridge_id":"review-kit","interfaces":[{"name":"iridium_imt","imei":"","egress_sealed":true}]}`
			case "gateway_absent":
				s.birth = `{"bridge_id":"review-kit","interfaces":[]}`
			case "lookup_error":
				s.err = errors.New("temporary declaration lookup failure")
			}
			after := process(2)
			t.Logf("scenario=%s opaque=%v compressed=%v text=%q wire_preserved=%v", scenario, after.Opaque, after.Compressed, after.Text, after.Wire == before.Wire)
			if !after.Opaque || after.Text != "" {
				t.Fatal("the same known ciphertext became readable when its declaration was unavailable")
			}
		})
	}
}
