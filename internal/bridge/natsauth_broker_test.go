package bridge

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
	"golang.org/x/crypto/bcrypt"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// The broker side of HeMB-NG contract C4.1 (MESHSAT-1584), against a real
// nats-server with the users file this package renders. Off by default; to run:
//
//	MESHSAT_NATS_WRITE_CONF=/tmp/x/nats.conf go test -run TestNATSLiveConf ./internal/bridge/
//	docker run --rm -d --name nats-c41 -p 127.0.0.1:18830:1883 -e NATS_MQTT_PASSWORD=hub-test \
//	  -v /tmp/x:/c nats@sha256:<the k8s/kustomization.yaml pin> -c /c/nats.conf
//	MESHSAT_NATS_MQTT=127.0.0.1:18830 go test -run TestNATSPermissionsOnARealBroker ./internal/bridge/
//
// The bridge passwords are test values, bcrypt-hashed into the file as the
// Hub's bridges table holds them; the Hub user's comes from the environment as
// in the cluster.
const (
	liveBridgePass = "c41-bridge-test"
	liveHubPass    = "hub-test"
)

func liveBridges(t *testing.T) []*store.Bridge {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(liveBridgePass), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return []*store.Bridge{
		{BridgeID: "b1", TenantID: "t_x", MQTTUsername: "b1", MQTTPasswordHash: string(hash)},
		{BridgeID: "b2", TenantID: "t_x", MQTTUsername: "b2", MQTTPasswordHash: string(hash)},
	}
}

// TestNATSLiveConf writes the broker config for the live test: JetStream and
// MQTT as in k8s/nats/configmap.yaml, single server, and the rendered users.
func TestNATSLiveConf(t *testing.T) {
	path := os.Getenv("MESHSAT_NATS_WRITE_CONF")
	if path == "" {
		t.Skip("MESHSAT_NATS_WRITE_CONF not set")
	}
	conf := "server_name: c41\nlisten: 127.0.0.1:4222\njetstream { store_dir: /tmp/js }\nmqtt { listen: 0.0.0.0:1883 }\n" +
		string(RenderNATSUsers(liveBridges(t), ns))
	if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
}

type liveClient struct {
	c    pahomqtt.Client
	mu   sync.Mutex
	got  []string
	lost bool
}

func dialLive(t *testing.T, addr, user, pass, id string) *liveClient {
	t.Helper()
	lc := &liveClient{}
	opts := pahomqtt.NewClientOptions().AddBroker("tcp://" + addr).SetClientID(id).
		SetUsername(user).SetPassword(pass).SetAutoReconnect(false).SetCleanSession(true).
		SetConnectionLostHandler(func(pahomqtt.Client, error) { lc.mu.Lock(); lc.lost = true; lc.mu.Unlock() })
	lc.c = pahomqtt.NewClient(opts)
	if tok := lc.c.Connect(); !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		t.Fatalf("%s: connect: %v", user, tok.Error())
	}
	return lc
}

func (lc *liveClient) subscribe(t *testing.T, filter string) error {
	t.Helper()
	tok := lc.c.Subscribe(filter, 1, func(_ pahomqtt.Client, m pahomqtt.Message) {
		lc.mu.Lock()
		lc.got = append(lc.got, m.Topic()+" "+string(m.Payload()))
		lc.mu.Unlock()
	})
	if !tok.WaitTimeout(10 * time.Second) {
		return fmt.Errorf("subscribe %s: timeout", filter)
	}
	if err := tok.Error(); err != nil {
		return err
	}
	if st, ok := tok.(*pahomqtt.SubscribeToken); ok {
		for _, code := range st.Result() {
			if code >= 0x80 {
				return fmt.Errorf("subscribe %s: refused (0x%02x)", filter, code)
			}
		}
	}
	return nil
}

func (lc *liveClient) received() []string {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return append([]string(nil), lc.got...)
}

func TestNATSPermissionsOnARealBroker(t *testing.T) {
	addr := os.Getenv("MESHSAT_NATS_MQTT")
	if addr == "" {
		t.Skip("MESHSAT_NATS_MQTT not set")
	}
	root := "meshsat/t_x/bridge/"
	hub := dialLive(t, addr, SharedNATSUser, liveHubPass, "c41-hub")
	defer hub.c.Disconnect(100)
	if err := hub.subscribe(t, root+"+/msg/#"); err != nil {
		t.Fatal(err)
	}

	// Each publish on its own session: a refused one may end the session, and
	// must not take the next one with it.
	for _, suffix := range []string{"out", "ack", "in", "status", "status/x"} {
		b := dialLive(t, addr, "b1", liveBridgePass, "c41-b1-"+strings.ReplaceAll(suffix, "/", "-"))
		tok := b.c.Publish(root+"b1/msg/"+suffix, 1, false, "from-b1")
		tok.WaitTimeout(5 * time.Second)
		t.Logf("b1 publish msg/%s: token error %v", suffix, tok.Error())
		b.c.Disconnect(100)
	}
	time.Sleep(2 * time.Second)
	got := strings.Join(hub.received(), "\n")
	for suffix, want := range map[string]bool{"out": true, "ack": true, "in": false, "status": false, "status/x": false} {
		if has := strings.Contains(got, root+"b1/msg/"+suffix+" from-b1"); has != want {
			t.Errorf("the Hub received b1's msg/%s: %v, want %v (got %q)", suffix, has, want, got)
		}
	}

	// What the Hub publishes on b1's msg/in reaches b1, and b1 cannot read b2's.
	b1 := dialLive(t, addr, "b1", liveBridgePass, "c41-b1-sub")
	defer b1.c.Disconnect(100)
	if err := b1.subscribe(t, root+"b1/msg/in"); err != nil {
		t.Fatalf("b1 cannot subscribe to its own msg/in: %v", err)
	}
	if err := b1.subscribe(t, root+"b2/msg/in"); err == nil {
		t.Error("b1 subscribed to b2's msg/in")
	} else {
		t.Logf("b1 on b2's msg/in: %v", err)
	}
	hub.c.Publish(root+"b1/msg/in", 1, false, "for-b1").WaitTimeout(5 * time.Second)
	hub.c.Publish(root+"b2/msg/in", 1, false, "for-b2").WaitTimeout(5 * time.Second)
	time.Sleep(2 * time.Second)
	in := strings.Join(b1.received(), "\n")
	if !strings.Contains(in, root+"b1/msg/in for-b1") || strings.Contains(in, "for-b2") {
		t.Errorf("b1 received %q, want only its own msg/in", in)
	}
}

// The broker side of C10 (MESHSAT-1584), run as TestNATSPermissionsOnARealBroker: a bridge
// publishes its own sos/event, never its sos/ack or hub/caps, which only the Hub publishes;
// it reads the Hub's retained hub/caps when it subscribes later, and its own sos/ack only.
func TestSOSPermissionsOnARealBroker(t *testing.T) {
	addr := os.Getenv("MESHSAT_NATS_MQTT")
	if addr == "" {
		t.Skip("MESHSAT_NATS_MQTT not set")
	}
	root := "meshsat/t_x/bridge/"
	hub := dialLive(t, addr, SharedNATSUser, liveHubPass, "c10-hub")
	defer hub.c.Disconnect(100)
	for _, f := range []string{root + "+/sos/#", root + "+/hub/#"} {
		if err := hub.subscribe(t, f); err != nil {
			t.Fatal(err)
		}
	}
	for _, suffix := range []string{"sos/event", "sos/ack", "hub/caps"} {
		b := dialLive(t, addr, "b1", liveBridgePass, "c10-b1-"+strings.ReplaceAll(suffix, "/", "-"))
		tok := b.c.Publish(root+"b1/"+suffix, 1, suffix == "hub/caps", "from-b1")
		tok.WaitTimeout(5 * time.Second)
		t.Logf("b1 publish %s: token error %v", suffix, tok.Error())
		b.c.Disconnect(100)
	}
	time.Sleep(2 * time.Second)
	got := strings.Join(hub.received(), "\n")
	for suffix, want := range map[string]bool{"sos/event": true, "sos/ack": false, "hub/caps": false} {
		if has := strings.Contains(got, root+"b1/"+suffix+" from-b1"); has != want {
			t.Errorf("the Hub received b1's %s: %v, want %v (got %q)", suffix, has, want, got)
		}
	}

	// The Hub's retained caps, then a bridge that subscribes after: it reads them. Its own
	// sos/ack reaches it; b2's does not.
	hub.c.Publish(root+"b1/hub/caps", 1, true, `caps-for-b1`).WaitTimeout(5 * time.Second)
	time.Sleep(time.Second)
	b1 := dialLive(t, addr, "b1", liveBridgePass, "c10-b1-sub")
	defer b1.c.Disconnect(100)
	for _, f := range []string{root + "b1/hub/caps", root + "b1/sos/ack"} {
		if err := b1.subscribe(t, f); err != nil {
			t.Fatalf("b1 cannot subscribe to its own %s: %v", f, err)
		}
	}
	if err := b1.subscribe(t, root+"b2/sos/ack"); err == nil {
		t.Error("b1 subscribed to b2's sos/ack")
	}
	hub.c.Publish(root+"b1/sos/ack", 1, false, "ack-for-b1").WaitTimeout(5 * time.Second)
	hub.c.Publish(root+"b2/sos/ack", 1, false, "ack-for-b2").WaitTimeout(5 * time.Second)
	time.Sleep(2 * time.Second)
	in := strings.Join(b1.received(), "\n")
	if !strings.Contains(in, root+"b1/hub/caps caps-for-b1") || !strings.Contains(in, root+"b1/sos/ack ack-for-b1") ||
		strings.Contains(in, "ack-for-b2") {
		t.Errorf("b1 received %q, want its retained caps and its own ack only", in)
	}
	// Clear the retained caps the test left.
	hub.c.Publish(root+"b1/hub/caps", 1, true, "").WaitTimeout(5 * time.Second)
}
