package satrelay

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// MTStatus is what an SBD relay MT publishes on the modem's mt/status topic,
// the shape cloudloop.MTStatusMessage has.
type MTStatus struct {
	ID        string `json:"id"`
	Channel   string `json:"channel"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	Timestamp string `json:"timestamp"`
}

// Recorder writes for an SBD relay MT what the Cloudloop path writes for an
// IMT one: the stored mt message, a message_sent audit row and a cost row when
// Rock7 took it, and the modem's mt/status either way.
type Recorder struct {
	Messages interface {
		InsertMessage(ctx context.Context, tenantID string, m *store.Message) error
	}
	Costs interface {
		InsertCostEntry(ctx context.Context, tenantID string, c *store.CostEntry) error
	}
	// Audit logs one audit row (audit.Service.Log with actor "system").
	Audit func(ctx context.Context, tenantID, action, detail string) error
	// Publish sends v on the tenant's mt/status topic for the modem.
	Publish    func(tenantID, imei string, v any) error
	CostPerMsg float64
	Now        func() time.Time
}

// Record is Sender.Record.
func (r *Recorder) Record(ctx context.Context, tenantID, imei string, wire []byte, mtID string, sendErr error) {
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	status, errMsg, id := "queued", "", "mt-"+mtID
	if sendErr != nil {
		status, errMsg = "failed", sendErr.Error()
	}
	if mtID == "" {
		id = "mt-relay-" + now.Format("20060102150405.000000000")
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := r.Messages.InsertMessage(wctx, tenantID, &store.Message{
		ID: id, DeviceIMEI: imei, Direction: "mt", Channel: "iridium",
		RawHex: hex.EncodeToString(wire), Status: status, Error: errMsg,
	}); err != nil {
		slog.Warn("satellite_relay: could not store the SBD MT", "imei", imei, "error", err)
	}
	if sendErr == nil {
		detail := fmt.Sprintf("imei=%s protocol=sbd provider=rock7 relay=true mt_id=%s bytes=%d", imei, mtID, len(wire))
		if err := r.Audit(wctx, tenantID, "message_sent", detail); err != nil {
			slog.Warn("satellite_relay: audit of the SBD MT failed", "imei", imei, "error", err)
		}
		if r.CostPerMsg > 0 {
			if err := r.Costs.InsertCostEntry(wctx, tenantID, &store.CostEntry{
				ID: uuid.NewString(), DeviceIMEI: imei, InterfaceType: "iridium_sbd", Direction: "mt",
				CostUSD: r.CostPerMsg, MessageID: mtID, Detail: fmt.Sprintf("relay bytes=%d", len(wire)),
			}); err != nil {
				slog.Warn("satellite_relay: cost of the SBD MT not recorded", "imei", imei, "error", err)
			}
		}
	}
	if err := r.Publish(tenantID, imei, MTStatus{
		ID: mtID, Channel: "iridium", Status: status, Error: errMsg, Timestamp: now.Format(time.RFC3339),
	}); err != nil {
		slog.Error("satellite_relay: publish mt/status failed", "imei", imei, "error", err)
	}
}
