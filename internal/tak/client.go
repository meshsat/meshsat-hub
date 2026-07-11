package tak

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Client manages a TCP/TLS connection to a TAK server for CoT XML streaming.
type Client struct {
	host    string
	port    int
	ssl     bool
	conn    net.Conn
	mu      sync.Mutex
	running atomic.Bool
	wg      sync.WaitGroup
	onEvent func(CotEvent) // callback for inbound CoT events
}

// NewClient creates a new TAK CoT TCP client.
func NewClient(host string, port int, ssl bool) *Client {
	return &Client{
		host: host,
		port: port,
		ssl:  ssl,
	}
}

// SetEventHandler sets the callback for inbound CoT events from the TAK server.
func (c *Client) SetEventHandler(fn func(CotEvent)) {
	c.onEvent = fn
}

// Connect establishes the TCP/TLS connection to the TAK server.
func (c *Client) Connect() error {
	conn, err := c.dial()
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	c.running.Store(true)

	c.wg.Add(1)
	go c.readLoop()

	slog.Info("tak: connected", "host", c.host, "port", c.port, "ssl", c.ssl)
	return nil
}

// Send writes a CoT event to the TAK server as newline-delimited XML.
func (c *Client) Send(ev CotEvent) error {
	data, err := MarshalCotEvent(ev)
	if err != nil {
		return fmt.Errorf("tak: marshal: %w", err)
	}

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("tak: not connected")
	}

	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = conn.Write(data)
	return err
}

// Disconnect closes the TAK server connection.
func (c *Client) Disconnect() {
	c.running.Store(false)
	c.mu.Lock()
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.mu.Unlock()
	c.wg.Wait()
	slog.Info("tak: disconnected")
}

// IsConnected returns true if the client has an active connection.
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil && c.running.Load()
}

func (c *Client) dial() (net.Conn, error) {
	addr := net.JoinHostPort(c.host, strconv.Itoa(c.port))
	if !c.ssl {
		return net.DialTimeout("tcp", addr, 10*time.Second)
	}
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 10 * time.Second},
		Config:    &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return dialer.DialContext(context.Background(), "tcp", addr)
}

func (c *Client) readLoop() {
	defer c.wg.Done()

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)

	for c.running.Load() {
		if err := conn.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
			break
		}
		if !scanner.Scan() {
			if !c.running.Load() {
				return
			}
			err := scanner.Err()
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					// A read-deadline timeout puts bufio.Scanner into a
					// permanent error state; reusing it makes Scan() return
					// false instantly and spins one CPU core at 100%.
					// Recreate the scanner to resume reading. [MESHSAT-697]
					scanner = bufio.NewScanner(conn)
					scanner.Buffer(make([]byte, 64*1024), 256*1024)
					continue // read timeout, keep going
				}
				slog.Warn("tak: read error", "error", err)
			} else {
				slog.Warn("tak: connection closed by server")
			}
			c.reconnect()
			return
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		ev, err := ParseCotEvent(line)
		if err != nil {
			slog.Debug("tak: parse inbound", "error", err)
			continue
		}

		// Skip keepalive events
		if ev.Type == TypeKeepalive || ev.Type == "t-x-c-t-r" {
			continue
		}

		if c.onEvent != nil {
			c.onEvent(*ev)
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

		conn, err := c.dial()
		if err != nil {
			slog.Warn("tak: reconnect failed", "error", err, "retry_in", wait)
			wait *= 2
			if wait > 5*time.Minute {
				wait = 5 * time.Minute
			}
			continue
		}

		c.mu.Lock()
		if c.conn != nil {
			_ = c.conn.Close()
		}
		c.conn = conn
		c.mu.Unlock()

		slog.Info("tak: reconnected")

		c.wg.Add(1)
		go c.readLoop()
		return
	}
}
