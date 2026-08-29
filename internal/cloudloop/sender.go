package cloudloop

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/codec"
	"github.com/meshsat/meshsat-hub/internal/compress"
	"github.com/meshsat/meshsat-hub/internal/fragment"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
)

// MTSendRequest is the JSON payload expected on meshsat/+/mt/send topics.
type MTSendRequest struct {
	Text       string `json:"text"`
	Channel    string `json:"channel,omitempty"`
	Priority   int    `json:"priority,omitempty"`
	Compress   bool   `json:"compress,omitempty"`
	Encrypt    bool   `json:"encrypt,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
	IMTTopic   string `json:"imt_topic,omitempty"`  // IMT only: PURPLE/PINK/RED/ORANGE/YELLOW/RAW
	RingStyle  string `json:"ring_style,omitempty"` // IMT only: NORMAL/URGENT/EXTENDED
}

const imtMTU = 100000 // IMT supports up to 100KB MT messages

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

// DeviceResolver looks up Cloudloop metadata for a device.
// Returns the Cloudloop thingID and whether the device uses IMT (9704) vs SBD (9603).
type DeviceResolver interface {
	// Resolve returns the Cloudloop thingID and true if the device uses IMT protocol.
	// If the device is unknown, thingID should be the IMEI itself and isIMT false.
	Resolve(imei string) (thingID string, isIMT bool)
}

// CostRecorder records satellite message costs.
type CostRecorder interface {
	InsertCostEntry(ctx context.Context, tenantID string, c *CostEntry) error
}

// CostEntry records the cost of a single satellite message send.
// Mirrors store.CostEntry to avoid import cycle.
type CostEntry struct {
	ID            string
	DeviceIMEI    string
	InterfaceType string
	Direction     string
	CostUSD       float64
	MessageID     string
	Detail        string
}

// Sender listens on MQTT for MT send requests and forwards them via Cloudloop.
type Sender struct {
	client       *Client
	mqtt         bus.MessageBus
	limiter      interface{ Allow(string, bool) bool }
	audit        *audit.Service
	resolver     DeviceResolver
	costRecorder CostRecorder
	costPerMsg   float64 // cost per message (USD), set via SetCostPerMessage
	maxRetries   int
	mtMTU        int           // MT payload MTU (default: 270)
	msgIDSeq     atomic.Uint64 // wrapping msg ID counter for fragmentation
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

// SetDeviceResolver attaches a resolver for looking up Cloudloop thingIDs
// and modem types. Without a resolver, the sender uses IMEI as thingID
// and defaults to SBD protocol.
func (s *Sender) SetDeviceResolver(r DeviceResolver) {
	s.resolver = r
}

// SetCostRecorder attaches a cost recorder for tracking satellite message costs.
func (s *Sender) SetCostRecorder(r CostRecorder) {
	s.costRecorder = r
}

// SetCostPerMessage sets the cost per message in USD for cost tracking.
func (s *Sender) SetCostPerMessage(cost float64) {
	s.costPerMsg = cost
}

// resolveDevice returns the Cloudloop thingID and protocol for a device IMEI.
func (s *Sender) resolveDevice(imei string) (thingID string, isIMT bool) {
	if s.resolver != nil {
		return s.resolver.Resolve(imei)
	}
	return imei, false
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

	// Resolve device: determine Cloudloop thingID and protocol (SBD vs IMT).
	thingID, isIMT := s.resolveDevice(deviceID)

	slog.Info("cloudloop: MT send request",
		"device", deviceID,
		"thing", thingID,
		"imt", isIMT,
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

	// IMT supports 100KB — fragmentation rarely needed. SBD uses 270-byte MT MTU.
	mtu := s.mtMTU
	if isIMT {
		mtu = imtMTU
	}

	// Fragment if payload exceeds MT MTU.
	frags := fragment.Fragment(data, mtu, s.nextMsgID())
	if frags != nil {
		slog.Info("cloudloop: fragmenting MT message",
			"device", deviceID, "total_bytes", len(data), "fragments", len(frags),
		)
		for i, frag := range frags {
			if err := s.sendPayload(thingID, isIMT, req.IMTTopic, req.RingStyle, frag, i, len(frags)); err != nil {
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
	if err := s.sendPayload(thingID, isIMT, req.IMTTopic, req.RingStyle, data, 0, 1); err != nil {
		s.publishStatus(deviceID, "", "failed", err.Error())
		return
	}
}

// sendPayload sends a single payload with exponential backoff retry.
// Uses the official Cloudloop Data API: SendSBD for 9603, SendIMT for 9704.
func (s *Sender) sendPayload(thingID string, isIMT bool, imtTopic, ringStyle string, data []byte, fragIdx, fragTotal int) error {
	protocol := "SBD"
	if isIMT {
		protocol = "IMT"
	}

	var lastErr error
	for attempt := 0; attempt <= s.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			slog.Info("cloudloop: retrying MT send",
				"attempt", attempt, "backoff", backoff, "thing", thingID,
				"protocol", protocol, "frag", fragIdx+1, "total", fragTotal,
			)
			time.Sleep(backoff)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var resp *MTResponse
		var err error
		if isIMT {
			resp, err = s.client.SendIMT(ctx, thingID, data, imtTopic, ringStyle)
		} else {
			resp, err = s.client.SendSBD(ctx, thingID, data)
		}
		cancel()

		if err != nil {
			lastErr = err
			slog.Warn("cloudloop: MT send failed",
				"error", err, "thing", thingID, "protocol", protocol, "attempt", attempt,
			)
			continue
		}

		if s.audit != nil {
			detail := fmt.Sprintf("thing=%s protocol=%s mt_id=%s bytes=%d frag=%d/%d",
				thingID, protocol, resp.ID, len(data), fragIdx+1, fragTotal)
			if err := s.audit.Log(context.Background(), "", "message_sent", "system", detail, ""); err != nil {
				slog.Warn("audit: failed to log message_sent", "error", err)
			}
		}
		if s.costRecorder != nil && s.costPerMsg > 0 {
			ifaceType := "iridium_sbd"
			if isIMT {
				ifaceType = "iridium_imt"
			}
			entry := &CostEntry{
				ID:            uuid.NewString(),
				DeviceIMEI:    thingID,
				InterfaceType: ifaceType,
				Direction:     "mt",
				CostUSD:       s.costPerMsg,
				MessageID:     resp.ID,
				Detail:        fmt.Sprintf("frag=%d/%d bytes=%d", fragIdx+1, fragTotal, len(data)),
			}
			if err := s.costRecorder.InsertCostEntry(context.Background(), "", entry); err != nil {
				slog.Warn("cost: failed to record cost entry", "error", err)
			}
		}
		if fragTotal == 1 {
			s.publishStatus(thingID, resp.ID, resp.Status, "")
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
