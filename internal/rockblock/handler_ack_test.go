package rockblock

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

type ackBus struct {
	mu   sync.Mutex
	msgs map[string][]byte
}

func (b *ackBus) Connect() error                                                { return nil }
func (b *ackBus) Disconnect()                                                   {}
func (b *ackBus) IsConnected() bool                                             { return true }
func (b *ackBus) Subscribe(string, byte, bus.MessageHandler) error              { return nil }
func (b *ackBus) QueueSubscribe(string, byte, string, bus.MessageHandler) error { return nil }
func (b *ackBus) Publish(topic string, _ byte, _ bool, p []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.msgs == nil {
		b.msgs = map[string][]byte{}
	}
	b.msgs[topic] = p
	return nil
}
func (b *ackBus) PublishJSON(topic string, qos byte, retained bool, v any) error {
	data, _ := json.Marshal(v)
	return b.Publish(topic, qos, retained, data)
}

// The Hub's receipt for an MO goes to the bridge that owns the modem, on that
// bridge's own topic, with the MOMSN the phone matches its message by; a modem
// no bridge owns gets none (MESHSAT-1246).
func TestHandler_MOReceiptGoesToTheOwningBridge(t *testing.T) {
	useTestKey(t)
	ctx := context.Background()
	db, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const owned, loose = "300434067943980", "300234065000077"
	for _, imei := range []string{owned, loose} {
		if err := db.CreateDevice(ctx, store.DefaultTenantID, &store.Device{IMEI: imei, Label: imei}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AssociateDeviceWithBridge(ctx, store.DefaultTenantID, owned, "msa-flaneur"); err != nil {
		t.Fatal(err)
	}

	b := &ackBus{}
	h := NewHandler(b, testSecret)
	h.SetStore(db)

	post := func(imei string, momsn int) {
		dataHex := hex.EncodeToString([]byte("tst"))
		form := url.Values{"imei": {imei}, "momsn": {jsonInt(momsn)}, "transmit_time": {"26-09-19 16:33:00"}, "data": {dataHex}}
		form.Set("JWT", signedJWT(t, jwt.MapClaims{"imei": imei, "momsn": momsn, "data": dataHex, "transmit_time": form.Get("transmit_time")}))
		req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("webhook %s: %d %s", imei, w.Code, w.Body.String())
		}
	}

	post(owned, 223)
	raw, ok := b.msgs["meshsat/bridge/msa-flaneur/mo/ack"]
	if !ok {
		t.Fatalf("no receipt on the owning bridge's topic; published: %v", keys(b.msgs))
	}
	var ack MOAck
	if err := json.Unmarshal(raw, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.IMEI != owned || ack.MOMSN != 223 || ack.Bearer != "sbd" || ack.ReceivedAt == "" {
		t.Errorf("receipt: %+v", ack)
	}

	before := len(b.msgs)
	post(loose, 5)
	for topic := range b.msgs {
		if strings.HasSuffix(topic, "/mo/ack") && !strings.Contains(topic, "msa-flaneur") {
			t.Errorf("a modem no bridge owns got a receipt on %s", topic)
		}
	}
	if len(b.msgs) <= before {
		t.Errorf("the unowned modem's MO was not processed at all")
	}
}

func jsonInt(n int) string { b, _ := json.Marshal(n); return string(b) }

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
