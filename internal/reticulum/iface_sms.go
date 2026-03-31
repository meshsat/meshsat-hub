package reticulum

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// SMSInterface implements Interface for SMS transport.
// RX: Twilio SMS webhook delivers packets via OnReceive.
// TX: sends via Twilio REST API (SMS client).
type SMSInterface struct {
	mu        sync.RWMutex
	handler   PacketHandler
	available bool
}

// NewSMSInterface creates an SMS Reticulum transport interface.
// SMS is RX-only for Reticulum relay (no outbound TX via SMS).
func NewSMSInterface() *SMSInterface {
	return &SMSInterface{available: true}
}

func (s *SMSInterface) Name() InterfaceType { return IfaceSMS }

func (s *SMSInterface) Cost() float64 { return InterfaceCost(IfaceSMS) }

// MTU returns the max payload for SMS. Standard SMS is 160 chars (7-bit)
// or 140 bytes binary; base64 encoding reduces effective binary payload.
func (s *SMSInterface) MTU() int { return 140 }

// Send is not supported for SMS — Reticulum TX over SMS is not implemented.
// SMS is RX-only: field devices can relay Reticulum packets to Hub via SMS,
// but Hub does not send Reticulum packets back over SMS.
func (s *SMSInterface) Send(_ context.Context, _ string, _ []byte) error {
	return fmt.Errorf("sms: outbound Reticulum TX not supported")
}

func (s *SMSInterface) IsAvailable() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.available
}

// SetAvailable updates the availability status.
func (s *SMSInterface) SetAvailable(avail bool) {
	s.mu.Lock()
	s.available = avail
	s.mu.Unlock()
}

// SetHandler registers a callback for inbound Reticulum packets.
func (s *SMSInterface) SetHandler(h PacketHandler) {
	s.mu.Lock()
	s.handler = h
	s.mu.Unlock()
}

// OnReceive dispatches an inbound packet from the SMS webhook.
func (s *SMSInterface) OnReceive(raw []byte) {
	s.mu.RLock()
	h := s.handler
	s.mu.RUnlock()

	if h == nil {
		slog.Warn("reticulum: sms packet received but no handler registered", "size", len(raw))
		return
	}
	slog.Debug("reticulum: sms MO packet received", "size", len(raw))
	h(IfaceSMS, raw)
}
