package tak

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	hubmqtt "github.com/cubeos-app/meshsat-hub/internal/mqtt"
	"github.com/cubeos-app/meshsat-hub/internal/protocol"
)

// FederationConfig holds TAK Federation v2 configuration.
type FederationConfig struct {
	Enabled        bool     `yaml:"enabled"`
	Port           int      `yaml:"port"`      // listen port, default 9001
	Peers          []string `yaml:"peers"`     // remote TAK servers (host:port)
	CertFile       string   `yaml:"cert_file"` // mTLS client/server cert
	KeyFile        string   `yaml:"key_file"`
	CAFile         string   `yaml:"ca_file"`           // trusted CA for peers
	CallsignPrefix string   `yaml:"callsign_prefix"`   // prefix for CoT callsigns
	CotStaleSec    int      `yaml:"cot_stale_seconds"` // CoT stale time
}

// Federation implements TAK Federation v2 — bidirectional CoT relay
// between the Hub's MQTT bus and remote TAK Servers.
type Federation struct {
	cfg      FederationConfig
	bus      FederationBus // interface for MQTT publish/subscribe
	listener net.Listener
	peers    []*federationPeer
	mu       sync.Mutex
	running  atomic.Bool
	wg       sync.WaitGroup //nolint:all
	msgsIn   atomic.Int64
	msgsOut  atomic.Int64
	cancel   context.CancelFunc
}

// FederationBus abstracts the MQTT message bus for federation.
type FederationBus interface {
	Publish(topic string, qos byte, retained bool, payload []byte) error
	Subscribe(topic string, qos byte, handler func(topic string, payload []byte)) error
}

type federationPeer struct {
	addr      string
	conn      net.Conn
	connected atomic.Bool
	wg        sync.WaitGroup
}

// NewFederation creates a TAK Federation v2 instance.
func NewFederation(cfg FederationConfig, bus FederationBus) *Federation {
	if cfg.Port == 0 {
		cfg.Port = 9001
	}
	if cfg.CallsignPrefix == "" {
		cfg.CallsignPrefix = "MESHSAT-HUB"
	}
	if cfg.CotStaleSec <= 0 {
		cfg.CotStaleSec = 600
	}
	return &Federation{
		cfg: cfg,
		bus: bus,
	}
}

// Start begins listening for inbound federation connections and connects to peers.
func (f *Federation) Start(ctx context.Context) error {
	ctx, f.cancel = context.WithCancel(ctx)
	f.running.Store(true)

	tlsCfg, err := f.buildTLSConfig()
	if err != nil {
		return fmt.Errorf("tak federation: TLS config: %w", err)
	}

	// Listen for inbound federation connections
	addr := ":" + strconv.Itoa(f.cfg.Port)
	listener, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		return fmt.Errorf("tak federation: listen %s: %w", addr, err)
	}
	f.listener = listener

	f.wg.Add(1)
	go f.acceptLoop(ctx)

	// Connect to configured peers
	for _, peerAddr := range f.cfg.Peers {
		f.wg.Add(1)
		go f.connectPeer(ctx, peerAddr, tlsCfg)
	}

	// Subscribe to MQTT for outbound federation — device telemetry topics
	mqttSubs := []string{
		"meshsat/+/position",
		"meshsat/+/sos",
		"meshsat/+/telemetry",
		"meshsat/+/mo/decoded",
		protocol.SubBridgeBirth,
		protocol.SubBridgeHealth,
		protocol.SubDeviceBirth,
	}
	for _, topic := range mqttSubs {
		if err := f.bus.Subscribe(topic, 0, f.handleMQTTForFederation); err != nil {
			slog.Warn("tak federation: subscribe failed", "topic", topic, "error", err)
		}
	}

	// Periodic dead peer cleanup
	f.wg.Add(1)
	go f.cleanupLoop(ctx)

	slog.Info("tak federation: started", "port", f.cfg.Port, "peers", len(f.cfg.Peers))
	return nil
}

// Stop shuts down all federation connections.
func (f *Federation) Stop() {
	f.running.Store(false)
	if f.cancel != nil {
		f.cancel()
	}
	if f.listener != nil {
		_ = f.listener.Close()
	}
	f.mu.Lock()
	for _, p := range f.peers {
		if p.conn != nil {
			_ = p.conn.Close()
		}
	}
	f.mu.Unlock()
	f.wg.Wait()
	slog.Info("tak federation: stopped")
}

// Relay sends CoT XML data to all connected federation peers.
// This is used by the TAK subscriber to forward events that were already
// converted from JSON to CoT XML.
func (f *Federation) Relay(data []byte) {
	if !f.running.Load() {
		return
	}
	f.sendToPeers(data)
}

// Stats returns federation message counts.
func (f *Federation) Stats() (in, out int64, peerCount int) {
	f.mu.Lock()
	pc := 0
	for _, p := range f.peers {
		if p.connected.Load() {
			pc++
		}
	}
	f.mu.Unlock()
	return f.msgsIn.Load(), f.msgsOut.Load(), pc
}

// ConnectedPeers returns the list of connected peer addresses.
func (f *Federation) ConnectedPeers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []string
	for _, p := range f.peers {
		if p.connected.Load() {
			result = append(result, p.addr)
		}
	}
	return result
}

func (f *Federation) buildTLSConfig() (*tls.Config, error) {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientAuth: tls.RequireAndVerifyClientCert,
	}

	if f.cfg.CertFile != "" && f.cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(f.cfg.CertFile, f.cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load cert: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	if f.cfg.CAFile != "" {
		caPEM, err := os.ReadFile(f.cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("invalid CA")
		}
		tlsCfg.RootCAs = pool
		tlsCfg.ClientCAs = pool
	}

	return tlsCfg, nil
}

func (f *Federation) acceptLoop(ctx context.Context) {
	defer f.wg.Done()
	for f.running.Load() {
		conn, err := f.listener.Accept()
		if err != nil {
			if !f.running.Load() {
				return
			}
			slog.Debug("tak federation: accept error", "error", err)
			continue
		}

		peer := &federationPeer{
			addr: conn.RemoteAddr().String(),
			conn: conn,
		}
		peer.connected.Store(true)

		f.mu.Lock()
		f.peers = append(f.peers, peer)
		f.mu.Unlock()

		slog.Info("tak federation: inbound peer", "addr", peer.addr)
		f.wg.Add(1)
		go f.readPeer(ctx, peer)
	}
}

func (f *Federation) connectPeer(ctx context.Context, addr string, tlsCfg *tls.Config) {
	defer f.wg.Done()
	wait := 5 * time.Second

	for f.running.Load() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		dialer := &tls.Dialer{
			NetDialer: &net.Dialer{Timeout: 10 * time.Second},
			Config:    tlsCfg,
		}
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			slog.Warn("tak federation: connect failed", "peer", addr, "retry_in", wait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			wait *= 2
			if wait > 5*time.Minute {
				wait = 5 * time.Minute
			}
			continue
		}

		peer := &federationPeer{addr: addr, conn: conn}
		peer.connected.Store(true)

		f.mu.Lock()
		f.peers = append(f.peers, peer)
		f.mu.Unlock()

		slog.Info("tak federation: connected to peer", "addr", addr)
		f.readPeer(ctx, peer)

		// If readPeer returns, peer disconnected — retry
		peer.connected.Store(false)
		wait = 5 * time.Second
	}
}

func (f *Federation) readPeer(ctx context.Context, peer *federationPeer) {
	defer func() {
		peer.connected.Store(false)
	}()

	scanner := bufio.NewScanner(peer.conn)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)

	for f.running.Load() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := peer.conn.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
			return
		}

		if !scanner.Scan() {
			if !f.running.Load() {
				return
			}
			err := scanner.Err()
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				slog.Debug("tak federation: peer read error", "peer", peer.addr, "error", err)
			}
			return
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		ev, err := ParseCotEvent(line)
		if err != nil {
			continue
		}

		// Skip keepalives
		if ev.Type == TypeKeepalive || ev.Type == "t-x-c-t-r" {
			continue
		}

		f.msgsIn.Add(1)

		// Relay inbound federation CoT to MQTT bus
		xmlData, err := MarshalCotEvent(*ev)
		if err != nil {
			continue
		}
		topic := fmt.Sprintf("meshsat/federation/%s/cot", ev.UID)
		if err := f.bus.Publish(topic, 0, false, xmlData); err != nil {
			slog.Debug("tak federation: publish to MQTT", "error", err)
		}
	}
}

// cleanupLoop periodically removes disconnected peers from the slice.
func (f *Federation) cleanupLoop(ctx context.Context) {
	defer f.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.mu.Lock()
			alive := make([]*federationPeer, 0, len(f.peers))
			for _, p := range f.peers {
				if p.connected.Load() {
					alive = append(alive, p)
				}
			}
			if len(alive) < len(f.peers) {
				slog.Debug("tak federation: cleaned up peers",
					"removed", len(f.peers)-len(alive), "remaining", len(alive))
			}
			f.peers = alive
			f.mu.Unlock()
		}
	}
}

// sendToPeers sends CoT XML to all connected federation peers.
func (f *Federation) sendToPeers(data []byte) {
	f.mu.Lock()
	peers := make([]*federationPeer, len(f.peers))
	copy(peers, f.peers)
	f.mu.Unlock()

	dataWithNewline := append(data, '\n')
	for _, p := range peers {
		if !p.connected.Load() || p.conn == nil {
			continue
		}
		if err := p.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
			continue
		}
		if _, err := p.conn.Write(dataWithNewline); err != nil {
			slog.Debug("tak federation: write to peer", "peer", p.addr, "error", err)
			p.connected.Store(false)
			continue
		}
		f.msgsOut.Add(1)
	}
}

// handleMQTTForFederation forwards MQTT events to federation peers as CoT XML.
func (f *Federation) handleMQTTForFederation(topic string, payload []byte) {
	if len(payload) == 0 {
		return
	}

	// If payload is already CoT XML, relay as-is.
	if payload[0] == '<' {
		f.sendToPeers(payload)
		return
	}

	// Determine topic type and convert JSON → CoT XML.
	switch {
	case strings.HasSuffix(topic, "/position"):
		f.federatePosition(topic, payload)
	case strings.HasSuffix(topic, "/sos"):
		f.federateSOS(topic, payload)
	case strings.HasSuffix(topic, "/telemetry"):
		f.federateTelemetry(topic, payload)
	case strings.HasSuffix(topic, "/mo/decoded"):
		f.federateMODecoded(topic, payload)
	case strings.Contains(topic, "/bridge/") && strings.HasSuffix(topic, "/birth"):
		// Could be bridge birth or device birth
		if strings.Contains(topic, "/device/") {
			f.federateDeviceBirth(topic, payload)
		} else {
			f.federateBridgeBirth(topic, payload)
		}
	case strings.Contains(topic, "/bridge/") && strings.HasSuffix(topic, "/health"):
		f.federateBridgeHealth(topic, payload)
	}
}

func (f *Federation) federatePosition(topic string, payload []byte) {
	deviceID := hubmqtt.ExtractDeviceID(topic)
	if deviceID == "" {
		return
	}
	var pos positionMessage
	if err := json.Unmarshal(payload, &pos); err != nil {
		return
	}
	if pos.Lat == 0 && pos.Lon == 0 {
		return
	}

	uid := "meshsat-" + deviceID
	callsign := f.cfg.CallsignPrefix + "-" + shortID(deviceID)
	source := pos.Source
	if source == "" {
		source = "gps"
	}

	ev := BuildPositionEvent(uid, callsign, pos.Lat, pos.Lon, pos.Alt, f.cfg.CotStaleSec, source)
	data, err := MarshalCotEvent(ev)
	if err != nil {
		return
	}
	f.sendToPeers(data)
}

func (f *Federation) federateSOS(topic string, payload []byte) {
	deviceID := hubmqtt.ExtractDeviceID(topic)
	if deviceID == "" {
		return
	}
	var sos sosMessage
	if err := json.Unmarshal(payload, &sos); err != nil {
		return
	}
	if !sos.Triggered {
		return
	}

	uid := "meshsat-" + deviceID
	callsign := f.cfg.CallsignPrefix + "-" + shortID(deviceID)

	ev := BuildSOSEvent(uid, callsign, sos.Lat, sos.Lon, f.cfg.CotStaleSec)
	data, err := MarshalCotEvent(ev)
	if err != nil {
		return
	}
	f.sendToPeers(data)
}

func (f *Federation) federateTelemetry(topic string, payload []byte) {
	deviceID := hubmqtt.ExtractDeviceID(topic)
	if deviceID == "" {
		return
	}
	var tel telemetryMessage
	if err := json.Unmarshal(payload, &tel); err != nil {
		return
	}

	uid := "meshsat-" + deviceID
	callsign := f.cfg.CallsignPrefix + "-" + shortID(deviceID)
	text := fmt.Sprintf("battery=%.0f%% temp=%.1fC humidity=%.0f%% pressure=%.0fhPa",
		tel.Battery, tel.Temperature, tel.Humidity, tel.Pressure)

	ev := BuildTelemetryEvent(uid, callsign, tel.Lat, tel.Lon, f.cfg.CotStaleSec, text)
	data, err := MarshalCotEvent(ev)
	if err != nil {
		return
	}
	f.sendToPeers(data)
}

func (f *Federation) federateMODecoded(topic string, payload []byte) {
	deviceID := hubmqtt.ExtractDeviceID(topic)
	if deviceID == "" {
		return
	}
	var mo moDecodedMessage
	if err := json.Unmarshal(payload, &mo); err != nil {
		return
	}

	uid := "meshsat-" + deviceID
	callsign := f.cfg.CallsignPrefix + "-" + shortID(deviceID)

	if mo.Text != "" {
		ev := BuildChatEvent(uid, callsign, mo.Text, f.cfg.CotStaleSec)
		data, err := MarshalCotEvent(ev)
		if err == nil {
			f.sendToPeers(data)
		}
	}
	if mo.IridiumLat != 0 || mo.IridiumLon != 0 {
		ev := BuildPositionEvent(uid, callsign, mo.IridiumLat, mo.IridiumLon, 0, f.cfg.CotStaleSec, "iridium_cep")
		data, err := MarshalCotEvent(ev)
		if err == nil {
			f.sendToPeers(data)
		}
	}
}

func (f *Federation) federateBridgeBirth(topic string, payload []byte) {
	var birth protocol.BridgeBirth
	if err := json.Unmarshal(payload, &birth); err != nil {
		return
	}
	ev := BuildBridgeEvent(birth, bridgeStaleSec)
	data, err := MarshalCotEvent(ev)
	if err != nil {
		return
	}
	f.sendToPeers(data)
}

func (f *Federation) federateDeviceBirth(topic string, payload []byte) {
	var device protocol.DeviceBirth
	if err := json.Unmarshal(payload, &device); err != nil {
		return
	}
	ev := BuildDeviceBirthEvent(device, f.cfg.CotStaleSec)
	data, err := MarshalCotEvent(ev)
	if err != nil {
		return
	}
	f.sendToPeers(data)
}

func (f *Federation) federateBridgeHealth(topic string, payload []byte) {
	var health protocol.BridgeHealth
	if err := json.Unmarshal(payload, &health); err != nil {
		return
	}
	// Health events without cached birth get defaults — acceptable for federation.
	ev := BuildBridgeHealthEvent(health, nil, bridgeStaleSec)
	data, err := MarshalCotEvent(ev)
	if err != nil {
		return
	}
	f.sendToPeers(data)
}
