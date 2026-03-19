package cloudloop

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/audit"
	"github.com/cubeos-app/meshsat-hub/internal/bus"
	"github.com/cubeos-app/meshsat-hub/internal/codec"
	"github.com/cubeos-app/meshsat-hub/internal/compress"
	"github.com/cubeos-app/meshsat-hub/internal/fragment"
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
	mqtt       bus.MessageBus
	limiter    interface{ Allow(string, bool) bool }
	audit      *audit.Service
	maxRetries int
	mtMTU      int           // MT payload MTU (default: 270)
	msgIDSeq   atomic.Uint64 // wrapping msg ID counter for fragmentation
}

// NewSender creates a new MT message sender.
func NewSender(client *Client, mqtt bus.MessageBus) *Sender {
	return &Sender{
		client:     client,
		mqtt:       mqtt,
		maxRetries: 3,
		mtMTU:      fragment.IridiumMT_MTU,
	}
}

// nextMsgID returns the next wrapping message ID (0-255).
func (s *Sender) nextMsgID() uint8 {
	return uint8(s.msgIDSeq.Add(1))
}

// SetAudit attaches an audit service for logging message_sent events.
func (s *Sender) SetAudit(a *audit.Service) {
	s.audit = a
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

	// Prepend protocol version byte before fragmentation.
	data = codec.PrependVersionByte(data)

	// Fragment if payload exceeds MT MTU.
	frags := fragment.Fragment(data, s.mtMTU, s.nextMsgID())
	if frags != nil {
		slog.Info("cloudloop: fragmenting MT message",
			"device", deviceID, "total_bytes", len(data), "fragments", len(frags),
		)
		for i, frag := range frags {
			if err := s.sendWithRetry(deviceID, frag, i, len(frags)); err != nil {
				slog.Error("cloudloop: fragment send failed, aborting remaining",
					"device", deviceID, "frag", i+1, "total", len(frags), "error", err,
				)
				s.publishStatus(deviceID, "", "failed",
					fmt.Sprintf("fragment %d/%d failed: %s", i+1, len(frags), err))
				return
			}
		}
		s.publishStatus(deviceID, "", "sent",
			fmt.Sprintf("sent %d fragments", len(frags)))
		return
	}

	// Single message — send directly.
	if err := s.sendWithRetry(deviceID, data, 0, 1); err != nil {
		s.publishStatus(deviceID, "", "failed", err.Error())
		return
	}
}

// sendWithRetry sends a single payload (possibly a fragment) with exponential backoff retry.
func (s *Sender) sendWithRetry(deviceID string, data []byte, fragIdx, fragTotal int) error {
	var lastErr error
	for attempt := 0; attempt <= s.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			slog.Info("cloudloop: retrying MT send",
				"attempt", attempt, "backoff", backoff, "device", deviceID,
				"frag", fragIdx+1, "total", fragTotal,
			)
			time.Sleep(backoff)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		resp, err := s.client.SendMT(ctx, deviceID, data)
		cancel()

		if err != nil {
			lastErr = err
			slog.Warn("cloudloop: MT send failed",
				"error", err, "device", deviceID, "attempt", attempt,
			)
			continue
		}

		if s.audit != nil {
			detail := fmt.Sprintf("device=%s mt_id=%s bytes=%d frag=%d/%d",
				deviceID, resp.ID, len(data), fragIdx+1, fragTotal)
			if err := s.audit.Log(context.Background(), "", "message_sent", "system", detail, ""); err != nil {
				slog.Warn("audit: failed to log message_sent", "error", err)
			}
		}
		if fragTotal == 1 {
			s.publishStatus(deviceID, resp.ID, resp.Status, "")
		}
		return nil
	}
	return lastErr
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
