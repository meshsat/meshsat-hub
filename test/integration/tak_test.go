//go:build integration

package integration

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/tak"
)

// mockTAKServer accepts CoT XML events over TCP.
type mockTAKServer struct {
	listener net.Listener
	received []tak.CotEvent
	mu       sync.Mutex
	wg       sync.WaitGroup
}

func newMockTAKServer(t *testing.T) *mockTAKServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("mock tak: listen: %v", err)
	}
	s := &mockTAKServer{listener: l}
	s.wg.Add(1)
	go s.accept()
	t.Cleanup(func() { s.close() })
	return s
}

func (s *mockTAKServer) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *mockTAKServer) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev tak.CotEvent
		if err := xml.Unmarshal(line, &ev); err != nil {
			continue
		}
		s.mu.Lock()
		s.received = append(s.received, ev)
		s.mu.Unlock()
	}
}

func (s *mockTAKServer) addr() string {
	return s.listener.Addr().String()
}

func (s *mockTAKServer) close() {
	s.listener.Close()
	s.wg.Wait()
}

func (s *mockTAKServer) waitEvents(n int, timeout time.Duration) []tak.CotEvent {
	deadline := time.After(timeout)
	for {
		s.mu.Lock()
		if len(s.received) >= n {
			out := make([]tak.CotEvent, len(s.received))
			copy(out, s.received)
			s.mu.Unlock()
			return out
		}
		s.mu.Unlock()
		select {
		case <-deadline:
			s.mu.Lock()
			out := make([]tak.CotEvent, len(s.received))
			copy(out, s.received)
			s.mu.Unlock()
			return out
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	port := 0
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	return host, port
}

// TestTAK_PositionMQTTToCoT verifies: MQTT position → Hub TAK subscriber → CoT PLI on mock TAK server
func TestTAK_PositionMQTTToCoT(t *testing.T) {
	// 1. Start embedded MQTT broker
	brokerAddr := testBroker(t)

	// 2. Start mock TAK server
	takSrv := newMockTAKServer(t)

	// 3. Hub MQTT client
	hubMQTT := hubmqtt.New(fmt.Sprintf("tcp://%s", brokerAddr), "hub-tak-test")
	if err := hubMQTT.Connect(); err != nil {
		t.Fatalf("mqtt connect: %v", err)
	}
	defer hubMQTT.Disconnect()

	// 4. TAK client connecting to mock server
	host, port := splitHostPort(t, takSrv.addr())
	takClient := tak.NewClient(host, port, false)
	if err := takClient.Connect(); err != nil {
		t.Fatalf("tak connect: %v", err)
	}
	defer takClient.Disconnect()

	// 5. TAK subscriber wiring MQTT → CoT
	sub := tak.NewSubscriber(hubMQTT, takClient, "TEST-HUB", 300)
	if err := sub.Start(); err != nil {
		t.Fatalf("tak subscriber start: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// 6. Publish a position to MQTT (simulating a field device)
	posJSON, _ := json.Marshal(map[string]interface{}{
		"lat":    52.3676,
		"lon":    4.9041,
		"source": "gps",
	})
	if err := hubMQTT.Publish("meshsat/300234063904190/position", 1, false, posJSON); err != nil {
		t.Fatalf("publish position: %v", err)
	}

	// 7. Wait for CoT event on mock TAK server
	events := takSrv.waitEvents(1, 3*time.Second)
	if len(events) == 0 {
		t.Fatal("expected at least 1 CoT event, got 0")
	}

	ev := events[0]
	if ev.Type != tak.TypePosition {
		t.Errorf("type: got %q, want %q", ev.Type, tak.TypePosition)
	}
	if ev.Point.Lat != 52.3676 {
		t.Errorf("lat: got %f, want 52.3676", ev.Point.Lat)
	}
	if ev.Detail == nil || ev.Detail.Contact == nil {
		t.Fatal("detail/contact nil")
	}
	if !strings.HasPrefix(ev.Detail.Contact.Callsign, "TEST-HUB-") {
		t.Errorf("callsign: got %q, want TEST-HUB-* prefix", ev.Detail.Contact.Callsign)
	}
}

// TestTAK_SOSMQTTToEmergencyCoT verifies: MQTT SOS → Hub TAK → CoT with emergency detail
func TestTAK_SOSMQTTToEmergencyCoT(t *testing.T) {
	brokerAddr := testBroker(t)
	takSrv := newMockTAKServer(t)

	hubMQTT := hubmqtt.New(fmt.Sprintf("tcp://%s", brokerAddr), "hub-sos-test")
	if err := hubMQTT.Connect(); err != nil {
		t.Fatalf("mqtt connect: %v", err)
	}
	defer hubMQTT.Disconnect()

	host, port := splitHostPort(t, takSrv.addr())
	takClient := tak.NewClient(host, port, false)
	if err := takClient.Connect(); err != nil {
		t.Fatalf("tak connect: %v", err)
	}
	defer takClient.Disconnect()

	sub := tak.NewSubscriber(hubMQTT, takClient, "TEST-HUB", 300)
	if err := sub.Start(); err != nil {
		t.Fatalf("tak subscriber start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// Publish SOS
	sosJSON, _ := json.Marshal(map[string]interface{}{
		"triggered": true,
		"lat":       51.5074,
		"lon":       -0.1278,
	})
	if err := hubMQTT.Publish("meshsat/device-sos/sos", 1, false, sosJSON); err != nil {
		t.Fatalf("publish sos: %v", err)
	}

	events := takSrv.waitEvents(1, 3*time.Second)
	if len(events) == 0 {
		t.Fatal("expected SOS CoT event")
	}

	ev := events[0]
	if ev.Detail == nil || ev.Detail.Emergency == nil {
		t.Fatal("expected emergency detail block")
	}
	if ev.Detail.Emergency.Type != "911 Alert" {
		t.Errorf("emergency type: got %q", ev.Detail.Emergency.Type)
	}
}

// TestTAK_MODecodedToChat verifies: MQTT mo/decoded with text → CoT chat event
func TestTAK_MODecodedToChat(t *testing.T) {
	brokerAddr := testBroker(t)
	takSrv := newMockTAKServer(t)

	hubMQTT := hubmqtt.New(fmt.Sprintf("tcp://%s", brokerAddr), "hub-mo-test")
	if err := hubMQTT.Connect(); err != nil {
		t.Fatalf("mqtt connect: %v", err)
	}
	defer hubMQTT.Disconnect()

	host, port := splitHostPort(t, takSrv.addr())
	takClient := tak.NewClient(host, port, false)
	if err := takClient.Connect(); err != nil {
		t.Fatalf("tak connect: %v", err)
	}
	defer takClient.Disconnect()

	sub := tak.NewSubscriber(hubMQTT, takClient, "TEST-HUB", 300)
	if err := sub.Start(); err != nil {
		t.Fatalf("tak subscriber start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// Publish MO decoded with text
	moJSON, _ := json.Marshal(map[string]interface{}{
		"imei":              "300234063904190",
		"text":              "All clear at checkpoint B",
		"iridium_latitude":  52.1621,
		"iridium_longitude": 4.5094,
	})
	if err := hubMQTT.Publish("meshsat/300234063904190/mo/decoded", 1, false, moJSON); err != nil {
		t.Fatalf("publish mo/decoded: %v", err)
	}

	// Should produce both a chat event AND a position event
	events := takSrv.waitEvents(2, 3*time.Second)
	if len(events) < 2 {
		t.Fatalf("expected 2 CoT events (chat + position), got %d", len(events))
	}

	hasChat := false
	hasPosition := false
	for _, ev := range events {
		if ev.Type == tak.TypeChat {
			hasChat = true
		}
		if ev.Type == tak.TypePosition {
			hasPosition = true
		}
	}
	if !hasChat {
		t.Error("expected chat CoT event (b-t-f)")
	}
	if !hasPosition {
		t.Error("expected position CoT event (a-f-G-U-C) from Iridium coordinates")
	}
}

// TestTAK_NullIslandFiltered verifies: position at 0,0 is not forwarded to TAK
func TestTAK_NullIslandFiltered(t *testing.T) {
	brokerAddr := testBroker(t)
	takSrv := newMockTAKServer(t)

	hubMQTT := hubmqtt.New(fmt.Sprintf("tcp://%s", brokerAddr), "hub-null-test")
	if err := hubMQTT.Connect(); err != nil {
		t.Fatalf("mqtt connect: %v", err)
	}
	defer hubMQTT.Disconnect()

	host, port := splitHostPort(t, takSrv.addr())
	takClient := tak.NewClient(host, port, false)
	if err := takClient.Connect(); err != nil {
		t.Fatalf("tak connect: %v", err)
	}
	defer takClient.Disconnect()

	sub := tak.NewSubscriber(hubMQTT, takClient, "TEST-HUB", 300)
	if err := sub.Start(); err != nil {
		t.Fatalf("tak subscriber start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// Publish position at 0,0 (null island — should be filtered)
	posJSON, _ := json.Marshal(map[string]interface{}{
		"lat": 0.0,
		"lon": 0.0,
	})
	hubMQTT.Publish("meshsat/device-null/position", 1, false, posJSON)

	events := takSrv.waitEvents(1, 1*time.Second)
	if len(events) > 0 {
		t.Errorf("expected 0 events for null island position, got %d", len(events))
	}
}
