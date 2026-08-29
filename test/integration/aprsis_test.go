//go:build integration

package integration

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/aprsis"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
)

// mockAPRSISServer simulates an APRS-IS server.
type mockAPRSISServer struct {
	listener net.Listener
	received []string
	mu       sync.Mutex
	wg       sync.WaitGroup
}

func newMockAPRSISServer(t *testing.T) *mockAPRSISServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("mock aprsis: listen: %v", err)
	}
	s := &mockAPRSISServer{listener: l}
	s.wg.Add(1)
	go s.accept()
	t.Cleanup(func() { s.close() })
	return s
}

func (s *mockAPRSISServer) accept() {
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

func (s *mockAPRSISServer) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	// Send server banner
	conn.Write([]byte("# mock APRS-IS server ready\r\n"))

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		// First non-empty line is the login — respond with verified
		if strings.HasPrefix(line, "user ") {
			conn.Write([]byte("# logresp CALL verified, server mock\r\n"))
			continue
		}
		// Capture all other lines as received packets
		s.mu.Lock()
		s.received = append(s.received, line)
		s.mu.Unlock()
	}
}

func (s *mockAPRSISServer) addr() string {
	return s.listener.Addr().String()
}

func (s *mockAPRSISServer) close() {
	s.listener.Close()
	s.wg.Wait()
}

func (s *mockAPRSISServer) waitPackets(n int, timeout time.Duration) []string {
	deadline := time.After(timeout)
	for {
		s.mu.Lock()
		if len(s.received) >= n {
			out := make([]string, len(s.received))
			copy(out, s.received)
			s.mu.Unlock()
			return out
		}
		s.mu.Unlock()
		select {
		case <-deadline:
			s.mu.Lock()
			out := make([]string, len(s.received))
			copy(out, s.received)
			s.mu.Unlock()
			return out
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestAPRSIS_IridiumPositionInjected verifies: MQTT position (source=iridium) → APRS-IS packet
func TestAPRSIS_IridiumPositionInjected(t *testing.T) {
	brokerAddr := testBroker(t)
	aprsSrv := newMockAPRSISServer(t)

	hubMQTT := hubmqtt.New(fmt.Sprintf("tcp://%s", brokerAddr), "hub-aprs-test")
	if err := hubMQTT.Connect(); err != nil {
		t.Fatalf("mqtt connect: %v", err)
	}
	defer hubMQTT.Disconnect()

	// APRS-IS client connecting to mock
	aprsClient := aprsis.NewClient(aprsSrv.addr(), "PA3XYZ", 10, "12345", "")
	if err := aprsClient.Connect(); err != nil {
		t.Fatalf("aprsis connect: %v", err)
	}
	defer aprsClient.Disconnect()

	sub := aprsis.NewSubscriber(hubMQTT, aprsClient, 1) // 1s coalesce for test speed
	if err := sub.Start(); err != nil {
		t.Fatalf("aprsis subscriber start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// Publish Iridium position
	posJSON, _ := json.Marshal(map[string]interface{}{
		"lat":    52.3676,
		"lon":    4.9041,
		"source": "iridium",
	})
	hubMQTT.Publish("meshsat/300234063904190/position", 1, false, posJSON)

	packets := aprsSrv.waitPackets(1, 3*time.Second)
	if len(packets) == 0 {
		t.Fatal("expected at least 1 APRS-IS packet")
	}

	pkt := packets[0]
	if !strings.Contains(pkt, "PA3XYZ-10>APMSHT") {
		t.Errorf("missing callsign/tocall: %s", pkt)
	}
	if !strings.Contains(pkt, "TCPIP*") {
		t.Errorf("missing TCPIP* path: %s", pkt)
	}
	if !strings.Contains(pkt, "N/") {
		t.Errorf("missing N hemisphere: %s", pkt)
	}
	if !strings.Contains(pkt, "MeshSat via iridium") {
		t.Errorf("missing source comment: %s", pkt)
	}
}

// TestAPRSIS_MeshPositionNotInjected verifies: non-satellite positions are NOT sent to APRS-IS
func TestAPRSIS_MeshPositionNotInjected(t *testing.T) {
	brokerAddr := testBroker(t)
	aprsSrv := newMockAPRSISServer(t)

	hubMQTT := hubmqtt.New(fmt.Sprintf("tcp://%s", brokerAddr), "hub-aprs-filter-test")
	if err := hubMQTT.Connect(); err != nil {
		t.Fatalf("mqtt connect: %v", err)
	}
	defer hubMQTT.Disconnect()

	aprsClient := aprsis.NewClient(aprsSrv.addr(), "PA3XYZ", 10, "12345", "")
	if err := aprsClient.Connect(); err != nil {
		t.Fatalf("aprsis connect: %v", err)
	}
	defer aprsClient.Disconnect()

	sub := aprsis.NewSubscriber(hubMQTT, aprsClient, 1)
	if err := sub.Start(); err != nil {
		t.Fatalf("aprsis subscriber start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// Publish GPS position (source=gps — NOT satellite, should be filtered)
	posJSON, _ := json.Marshal(map[string]interface{}{
		"lat":    52.3676,
		"lon":    4.9041,
		"source": "gps",
	})
	hubMQTT.Publish("meshsat/device-mesh/position", 1, false, posJSON)

	packets := aprsSrv.waitPackets(1, 1*time.Second)
	if len(packets) > 0 {
		t.Errorf("expected 0 packets for non-satellite position, got %d: %v", len(packets), packets)
	}
}

// TestAPRSIS_RateLimited verifies: only 1 position per device per coalesce window
func TestAPRSIS_RateLimited(t *testing.T) {
	brokerAddr := testBroker(t)
	aprsSrv := newMockAPRSISServer(t)

	hubMQTT := hubmqtt.New(fmt.Sprintf("tcp://%s", brokerAddr), "hub-aprs-rate-test")
	if err := hubMQTT.Connect(); err != nil {
		t.Fatalf("mqtt connect: %v", err)
	}
	defer hubMQTT.Disconnect()

	aprsClient := aprsis.NewClient(aprsSrv.addr(), "PA3XYZ", 10, "12345", "")
	if err := aprsClient.Connect(); err != nil {
		t.Fatalf("aprsis connect: %v", err)
	}
	defer aprsClient.Disconnect()

	sub := aprsis.NewSubscriber(hubMQTT, aprsClient, 60) // 60s coalesce — tight window
	if err := sub.Start(); err != nil {
		t.Fatalf("aprsis subscriber start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// Publish 3 positions rapidly for the same device
	for i := 0; i < 3; i++ {
		posJSON, _ := json.Marshal(map[string]interface{}{
			"lat":    52.3676 + float64(i)*0.001,
			"lon":    4.9041,
			"source": "iridium",
		})
		hubMQTT.Publish("meshsat/same-device/position", 1, false, posJSON)
		time.Sleep(100 * time.Millisecond)
	}

	time.Sleep(500 * time.Millisecond)
	packets := aprsSrv.waitPackets(1, 1*time.Second)

	// Only 1 should get through (rate limited)
	if len(packets) != 1 {
		t.Errorf("expected exactly 1 packet (rate limited), got %d", len(packets))
	}
}

// TestAPRSIS_MODecodedWithIridiumCoords verifies: mo/decoded with Iridium lat/lon → APRS-IS position
func TestAPRSIS_MODecodedWithIridiumCoords(t *testing.T) {
	brokerAddr := testBroker(t)
	aprsSrv := newMockAPRSISServer(t)

	hubMQTT := hubmqtt.New(fmt.Sprintf("tcp://%s", brokerAddr), "hub-aprs-mo-test")
	if err := hubMQTT.Connect(); err != nil {
		t.Fatalf("mqtt connect: %v", err)
	}
	defer hubMQTT.Disconnect()

	aprsClient := aprsis.NewClient(aprsSrv.addr(), "PA3XYZ", 10, "12345", "")
	if err := aprsClient.Connect(); err != nil {
		t.Fatalf("aprsis connect: %v", err)
	}
	defer aprsClient.Disconnect()

	sub := aprsis.NewSubscriber(hubMQTT, aprsClient, 1)
	if err := sub.Start(); err != nil {
		t.Fatalf("aprsis subscriber start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	moJSON, _ := json.Marshal(map[string]interface{}{
		"imei":              "300234063904190",
		"text":              "All clear",
		"iridium_latitude":  51.5074,
		"iridium_longitude": -0.1278,
	})
	hubMQTT.Publish("meshsat/300234063904190/mo/decoded", 1, false, moJSON)

	packets := aprsSrv.waitPackets(1, 3*time.Second)
	if len(packets) == 0 {
		t.Fatal("expected APRS-IS packet from MO decoded")
	}

	if !strings.Contains(packets[0], "Iridium SBD") {
		t.Errorf("missing Iridium SBD comment: %s", packets[0])
	}
	if !strings.Contains(packets[0], "All clear") {
		t.Errorf("missing message text in comment: %s", packets[0])
	}
}
