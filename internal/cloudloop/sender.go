package cloudloop

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/compress"
	hubmqtt "github.com/cubeos-app/meshsat-hub/internal/mqtt"
)

// MTSendRequest is the JSON payload expected on meshsat/+/mt/send topics.
type MTSendRequest struct {
	Text       string `json:"text"`
	Channel    string `json:"channel,omitempty"`
	Priority   int    `json:"priority,omitempty"`
	Compress   bool   `json:"compress,omitempty"`
	Encrypt    bool   `json:"encrypt,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// MTStatusMessage is published to meshsat/{device_id}/mt/status.
type MTStatusMessage struct {
	ID        string `json:"id"`
	Channel   string `json:"channel"`
	Status    string `json:"status"`
	MOStatus  int    `json:"mo_status,omitempty"`
	MTStatus  int    `json:"mt_status,omitempty"`
	Error     string `json:"error,omitempty"`
	Timestamp string `json:"timestamp"`
}

// Sender listens on MQTT for MT send requests and forwards them via Cloudloop.
type Sender struct {
	client     *Client
	mqtt       *hubmqtt.Client
	limiter    interface{ Allow(string, bool) bool }
	maxRetries int
}

// NewSender creates a new MT message sender.
func NewSender(client *Client, mqtt *hubmqtt.Client) *Sender {
	return &Sender{
		client:     client,
		mqtt:       mqtt,
		maxRetries: 3,
	}
}

// SetRateLimiter attaches a per-device rate limiter. If set, sends are
// checked against the limiter before calling Cloudloop. SOS messages bypass.
func (s *Sender) SetRateLimiter(l interface{ Allow(string, bool) bool }) {
	s.limiter = l
}

// Start subscribes to MT send topics and begins processing.
func (s *Sender) Start() error {
	return s.mqtt.Subscribe(hubmqtt.TopicMTSendWildcard(), 1, s.handleMTSend)
}

func (s *Sender) handleMTSend(topic string, payload []byte) {
	deviceID := hubmqtt.ExtractDeviceID(topic)
	if deviceID == "" {
		slog.Warn("cloudloop: could not extract device ID from topic", "topic", topic)
		return
	}

	var req MTSendRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		slog.Warn("cloudloop: invalid MT send request", "error", err, "device", deviceID)
		s.publishStatus(deviceID, "", "failed", "invalid request: "+err.Error())
		return
	}

	slog.Info("cloudloop: MT send request",
		"device", deviceID,
		"text_len", len(req.Text),
		"compress", req.Compress,
	)

	// Rate limit check (SOS messages bypass).
	isSOS := req.Priority >= 9
	if s.limiter != nil && !s.limiter.Allow(deviceID, isSOS) {
		slog.Warn("cloudloop: MT send rate-limited", "device", deviceID)
		s.publishStatus(deviceID, "", "rate_limited", "device rate limit exceeded")
		return
	}

	// Prepare payload.
	data := []byte(req.Text)
	if req.Compress {
		data = compress.Compress(data)
	}

	// Send with retry.
	var lastErr error
	for attempt := 0; attempt <= s.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			slog.Info("cloudloop: retrying MT send", "attempt", attempt, "backoff", backoff, "device", deviceID)
			time.Sleep(backoff)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		resp, err := s.client.SendMT(ctx, deviceID, data)
		cancel()

		if err != nil {
			lastErr = err
			slog.Warn("cloudloop: MT send failed", "error", err, "device", deviceID, "attempt", attempt)
			continue
		}

		// Success.
		s.publishStatus(deviceID, resp.ID, resp.Status, "")
		return
	}

	// All retries exhausted.
	errMsg := ""
	if lastErr != nil {
		errMsg = lastErr.Error()
	}
	s.publishStatus(deviceID, "", "failed", errMsg)
}

func (s *Sender) publishStatus(deviceID, mtID, status, errMsg string) {
	msg := MTStatusMessage{
		ID:        mtID,
		Channel:   "iridium",
		Status:    status,
		Error:     errMsg,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	if err := s.mqtt.PublishJSON(hubmqtt.TopicMTStatus(deviceID), 1, false, msg); err != nil {
		slog.Error("cloudloop: publish mt/status failed", "error", err, "device", deviceID)
	}
}
