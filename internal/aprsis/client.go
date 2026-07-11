package aprsis

import (
	"bufio"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Client manages a TCP connection to an APRS-IS server.
type Client struct {
	server   string // e.g. "euro.aprs2.net:14580"
	callsign string // e.g. "PA3XYZ"
	ssid     int    // e.g. 10 (IGate convention)
	passcode string // APRS-IS verification passcode
	filter   string // server-side filter (e.g. "r/52/5/500")

	conn    net.Conn
	mu      sync.Mutex
	running atomic.Bool
	wg      sync.WaitGroup

	onPacket func(line string) // callback for inbound APRS-IS packets
}

// NewClient creates a new APRS-IS client.
func NewClient(server, callsign string, ssid int, passcode, filter string) *Client {
	if filter == "" {
		filter = "r/52/5/500" // default: 500km radius around Netherlands
	}
	return &Client{
		server:   server,
		callsign: callsign,
		ssid:     ssid,
		passcode: passcode,
		filter:   filter,
	}
}

// SetPacketHandler sets the callback for inbound APRS-IS packets.
func (c *Client) SetPacketHandler(fn func(string)) {
	c.onPacket = fn
}

// Connect establishes the connection and sends the login packet.
func (c *Client) Connect() error {
	conn, err := net.DialTimeout("tcp", c.server, 10*time.Second)
	if err != nil {
		return fmt.Errorf("aprsis: dial %s: %w", c.server, err)
	}

	// Read server banner
	scanner := bufio.NewScanner(conn)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if scanner.Scan() {
		slog.Debug("aprsis: server banner", "banner", scanner.Text())
	}

	// Send login
	login := fmt.Sprintf("user %s-%d pass %s vers MeshSat-Hub 1.0 filter %s\r\n",
		c.callsign, c.ssid, c.passcode, c.filter)
	if _, err := conn.Write([]byte(login)); err != nil {
		_ = conn.Close()
		return fmt.Errorf("aprsis: login: %w", err)
	}

	// Read login response
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if scanner.Scan() {
		resp := scanner.Text()
		slog.Info("aprsis: login response", "response", resp)
		if strings.Contains(resp, "unverified") {
			slog.Warn("aprsis: login unverified — read-only mode (check passcode)")
		}
	}

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	c.running.Store(true)

	c.wg.Add(1)
	go c.readLoop()

	slog.Info("aprsis: connected", "server", c.server, "callsign", fmt.Sprintf("%s-%d", c.callsign, c.ssid))
	return nil
}

// Send transmits a raw APRS-IS packet line (without trailing \r\n).
func (c *Client) Send(packet string) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("aprsis: not connected")
	}

	line := packet + "\r\n"
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	_, err := conn.Write([]byte(line))
	if err != nil {
		slog.Warn("aprsis: send failed", "error", err)
	}
	return err
}

// Disconnect closes the connection.
func (c *Client) Disconnect() {
	c.running.Store(false)
	c.mu.Lock()
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.mu.Unlock()
	c.wg.Wait()
	slog.Info("aprsis: disconnected")
}

// IsConnected returns true if connected.
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil && c.running.Load()
}

// FormatCallsign returns the full callsign with SSID.
func (c *Client) FormatCallsign() string {
	return fmt.Sprintf("%s-%d", c.callsign, c.ssid)
}

func (c *Client) readLoop() {
	defer c.wg.Done()

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	scanner := bufio.NewScanner(conn)
	// Clear the login-phase read deadline: the loop must block. A deadline
	// expiry poisons bufio.Scanner (sticky error -> busy spin,
	// IFRNLLEI01PRD-905); dead peers surface via OS TCP keepalive instead.
	_ = conn.SetReadDeadline(time.Time{})
	for c.running.Load() {
		if !scanner.Scan() {
			if !c.running.Load() {
				return
			}
			if err := scanner.Err(); err != nil {
				slog.Warn("aprsis: read error", "error", err)
			} else {
				slog.Warn("aprsis: connection closed by server")
			}
			c.reconnect()
			return
		}

		line := scanner.Text()
		if line == "" || line[0] == '#' {
			continue // skip comments/server messages
		}

		if c.onPacket != nil {
			c.onPacket(line)
		}
	}
}

func (c *Client) reconnect() {
	wait := 5 * time.Second
	for c.running.Load() {
		time.Sleep(wait)
		if !c.running.Load() {
			return
		}

		if err := c.Connect(); err != nil {
			slog.Warn("aprsis: reconnect failed", "error", err, "retry_in", wait)
			wait *= 2
			if wait > 5*time.Minute {
				wait = 5 * time.Minute
			}
			continue
		}
		return
	}
}

// FormatPosition creates an APRS-IS position packet string.
// Format: CALLSIGN-SSID>APMSHT,TCPIP*:!DDMM.MMN/DDDMM.MMW-comment
func FormatPosition(callsign string, ssid int, lat, lon float64, comment string) string {
	latDir := byte('N')
	if lat < 0 {
		latDir = 'S'
		lat = -lat
	}
	latDeg := int(lat)
	latMin := (lat - float64(latDeg)) * 60.0

	lonDir := byte('E')
	if lon < 0 {
		lonDir = 'W'
		lon = math.Abs(lon)
	}
	lonDeg := int(lon)
	lonMin := (lon - float64(lonDeg)) * 60.0

	return fmt.Sprintf("%s-%d>APMSHT,TCPIP*:!%02d%05.2f%c/%03d%05.2f%c-%s",
		callsign, ssid,
		latDeg, latMin, latDir,
		lonDeg, lonMin, lonDir,
		comment,
	)
}
