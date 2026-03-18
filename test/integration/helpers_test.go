//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"

	"github.com/cubeos-app/meshsat-hub/internal/cloudloop"
	"github.com/cubeos-app/meshsat-hub/internal/fragment"
	hubmqtt "github.com/cubeos-app/meshsat-hub/internal/mqtt"
	"github.com/cubeos-app/meshsat-hub/internal/rockblock"
	"github.com/cubeos-app/meshsat-hub/internal/sos"
	"github.com/go-chi/chi/v5"
)

// testBroker starts an embedded MQTT broker on a random port.
func testBroker(t *testing.T) string {
	t.Helper()

	broker := mochi.New(nil)
	_ = broker.AddHook(new(auth.AllowHook), nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	tcp := listeners.NewTCP(listeners.Config{ID: "test", Address: addr})
	if err := broker.AddListener(tcp); err != nil {
		t.Fatalf("add listener: %v", err)
	}

	go broker.Serve()

	// Wait for broker to accept connections.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Cleanup(func() { broker.Close() })
	return addr
}

// testMQTTClient creates a paho MQTT client connected to the embedded broker.
func testMQTTClient(t *testing.T, addr, clientID string) pahomqtt.Client {
	t.Helper()

	opts := pahomqtt.NewClientOptions().
		AddBroker(fmt.Sprintf("tcp://%s", addr)).
		SetClientID(clientID).
		SetAutoReconnect(false).
		SetCleanSession(true)

	c := pahomqtt.NewClient(opts)
	tok := c.Connect()
	if !tok.WaitTimeout(5 * time.Second) {
		t.Fatalf("mqtt connect timeout")
	}
	if tok.Error() != nil {
		t.Fatalf("mqtt connect: %v", tok.Error())
	}
	t.Cleanup(func() { c.Disconnect(500) })
	return c
}

// mqttCollector subscribes to MQTT topics and collects messages.
type mqttCollector struct {
	mu       sync.Mutex
	messages []mqttMsg
	notify   chan struct{}
}

type mqttMsg struct {
	Topic   string
	Payload json.RawMessage
}

func newCollector(t *testing.T, client pahomqtt.Client, topic string) *mqttCollector {
	t.Helper()
	c := &mqttCollector{notify: make(chan struct{}, 64)}
	tok := client.Subscribe(topic, 1, func(_ pahomqtt.Client, msg pahomqtt.Message) {
		c.mu.Lock()
		c.messages = append(c.messages, mqttMsg{
			Topic:   msg.Topic(),
			Payload: json.RawMessage(msg.Payload()),
		})
		c.mu.Unlock()
		select {
		case c.notify <- struct{}{}:
		default:
		}
	})
	if !tok.WaitTimeout(5 * time.Second) {
		t.Fatalf("subscribe timeout on %s", topic)
	}
	if tok.Error() != nil {
		t.Fatalf("subscribe %s: %v", topic, tok.Error())
	}
	return c
}

func (c *mqttCollector) wait(n int, timeout time.Duration) []mqttMsg {
	deadline := time.After(timeout)
	for {
		c.mu.Lock()
		if len(c.messages) >= n {
			out := make([]mqttMsg, len(c.messages))
			copy(out, c.messages)
			c.mu.Unlock()
			return out
		}
		c.mu.Unlock()
		select {
		case <-deadline:
			c.mu.Lock()
			out := make([]mqttMsg, len(c.messages))
			copy(out, c.messages)
			c.mu.Unlock()
			return out
		case <-c.notify:
		}
	}
}

// testEnv holds all components needed for integration tests.
type testEnv struct {
	BrokerAddr    string
	HubMQTT       *hubmqtt.Client
	Router        http.Handler
	CloudloopSrv  *httptest.Server
	CloudloopReqs []cloudloopReq // captured MT requests
	mu            sync.Mutex
}

type cloudloopReq struct {
	IMEI string
	Data string // hex-encoded
}

// testStack wires up the full integration test environment:
// embedded MQTT broker, hub MQTT client, mock Cloudloop API, RockBLOCK handler, MT sender.
func testStack(t *testing.T) *testEnv {
	t.Helper()

	env := &testEnv{}

	// 1. Embedded MQTT broker.
	env.BrokerAddr = testBroker(t)

	// 2. Hub MQTT client.
	env.HubMQTT = hubmqtt.New(
		fmt.Sprintf("tcp://%s", env.BrokerAddr),
		"meshsat-hub-test",
	)
	if err := env.HubMQTT.Connect(); err != nil {
		t.Fatalf("hub mqtt connect: %v", err)
	}
	t.Cleanup(func() { env.HubMQTT.Disconnect() })

	// 3. Mock Cloudloop API.
	env.CloudloopSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sbd/mt" && r.Method == "POST" {
			var req cloudloop.MTRequest
			json.NewDecoder(r.Body).Decode(&req)
			env.mu.Lock()
			env.CloudloopReqs = append(env.CloudloopReqs, cloudloopReq{
				IMEI: req.IMEI,
				Data: req.Data,
			})
			env.mu.Unlock()
			json.NewEncoder(w).Encode(cloudloop.MTResponse{
				ID:     fmt.Sprintf("mt-%d", time.Now().UnixNano()),
				Status: "queued",
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(func() { env.CloudloopSrv.Close() })

	// 4. Cloudloop client + MT sender.
	clClient := cloudloop.NewClient(env.CloudloopSrv.URL, "test-api-key")
	mtSender := cloudloop.NewSender(clClient, env.HubMQTT)
	if err := mtSender.Start(); err != nil {
		t.Fatalf("start MT sender: %v", err)
	}

	// 5. RockBLOCK handler with fragment reassembler (mirrors main.go).
	rbHandler := rockblock.NewHandler(env.HubMQTT, "test-secret")
	reassembler := fragment.NewReassembler(5 * time.Minute)
	rbHandler.SetReassembler(reassembler)

	// 6. SOS detector (subscribes to mo/decoded, publishes to sos topic).
	sosDetector := sos.NewDetector(env.HubMQTT, nil, nil, "", "")
	if err := sosDetector.Start(); err != nil {
		t.Fatalf("start SOS detector: %v", err)
	}

	// 7. Build chi router (mirrors main.go).
	r := chi.NewRouter()
	r.Post("/api/webhook/rockblock", rbHandler.ServeHTTP)
	env.Router = r

	// Give MQTT subscriptions time to propagate.
	time.Sleep(200 * time.Millisecond)

	return env
}

// getCloudloopReqs returns captured Cloudloop MT requests.
func (e *testEnv) getCloudloopReqs() []cloudloopReq {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]cloudloopReq, len(e.CloudloopReqs))
	copy(out, e.CloudloopReqs)
	return out
}
