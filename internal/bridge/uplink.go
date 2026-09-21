package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"log/slog"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/audit"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// UplinkStore is the slice of the store the uplink sink needs.
type UplinkStore interface {
	SetBridgeHealth(ctx context.Context, tenantID string, bridgeID string, health string) error
	TouchBridgeLastSeen(ctx context.Context, tenantID string, bridgeID string) error
	SetBridgeLastReport(ctx context.Context, tenantID string, bridgeID string, bearer string, at time.Time) error
}

// UplinkPublisher publishes one MQTT message (the webhooks' publish helper).
type UplinkPublisher func(topic string, qos byte, retained bool, v any)

// UplinkSink turns a bridge's satellite/SMS uplink frames (position, SOS,
// health; magic "MS", internal/bridge/satdecoder.go) into the same fleet
// state an MQTT bridge produces, whichever bearer carried the frame
// (MESHSAT-964 B): the position lands on the position topic (stored by the
// position subscriber), a health frame updates the per-interface status on
// the Fleet page, an SOS frame raises the same alert as an MQTT SOS through
// mo/decoded with sos=true, and the bridge records the bearer and time of
// its last report.
type UplinkSink struct {
	store   UplinkStore
	publish UplinkPublisher
	audit   *audit.Service
	actor   string // audit actor, e.g. "sms_webhook"
	now     func() time.Time
	tenants *tenancy.Resolver // bridge id -> owning tenant; nil disables the check
}

// SetTenants lets the sink check that a bridge belongs to the tenant whose
// modem or phone carried the frame. The bridge id comes out of the uplink
// payload, which is attacker-controlled, so without this a device in one
// tenant can drive bridge rows and SOS topics for a bridge id owned by another
// (MESHSAT-975).
func (u *UplinkSink) SetTenants(r *tenancy.Resolver) *UplinkSink { u.tenants = r; return u }

// ownsBridge reports whether tenantID may write to bridgeID. An unregistered
// bridge id belongs to whoever presented it; a registered one belongs to its
// owner and nobody else.
func (u *UplinkSink) ownsBridge(ctx context.Context, tenantID, bridgeID string) bool {
	if u.tenants == nil || bridgeID == "" {
		return true
	}
	if owner := u.tenants.ForBridgeTopic(ctx, bridgeID, tenantID); owner != tenantID {
		slog.Warn("uplink: bridge belongs to another tenant, refusing", "bridge_id", bridgeID, "tenant", tenantID, "owner", owner)
		return false
	}
	return true
}

// bridgeLister is the optional part of the store that lets the sink recognise a
// bridge id that arrived cut short.
type bridgeLister interface {
	ListBridges(ctx context.Context, tenantID string) ([]*store.Bridge, error)
}

// resolveBridgeID turns the id inside a frame into a bridge this tenant has.
//
// The field kits' encoder cut the id at 16 bytes, so "nllei01tesseract01"
// arrived as "nllei01tesseract" (2026-09-21). The health update then ran against
// a bridge that does not exist: zero rows, no error, no log line, and the one
// time the fallback uplink was exercised for real it changed nothing and said
// nothing. A frame is tiny and travels over SMS or a satellite modem, so a
// sender shortening an id is not far-fetched either.
//
// An exact match wins. Otherwise, when exactly ONE bridge of the SAME tenant
// starts with the id, that is the bridge, and the log says so. The tenant comes
// from the modem or phone that carried the frame, never from the frame, so this
// cannot reach into another tenant. Anything else is left as it came, with a
// warning that nothing was updated.
func (u *UplinkSink) resolveBridgeID(ctx context.Context, tenantID, id, bearer string) string {
	lister, ok := u.store.(bridgeLister)
	if !ok || id == "" {
		return id
	}
	bridges, err := lister.ListBridges(ctx, tenantID)
	if err != nil {
		return id // the write below still has the ownership check; this only loses the hint
	}
	var matches []string
	for _, b := range bridges {
		if b.BridgeID == id {
			return id
		}
		if len(id) >= 8 && strings.HasPrefix(b.BridgeID, id) {
			matches = append(matches, b.BridgeID)
		}
	}
	if len(matches) == 1 {
		slog.Info("uplink: frame carries a shortened bridge id, matched to the one bridge it fits",
			"frame_id", id, "bridge_id", matches[0], "bearer", bearer)
		return matches[0]
	}
	slog.Warn("uplink: frame names a bridge this tenant does not have; nothing will be updated",
		"frame_id", id, "tenant", tenantID, "bearer", bearer, "candidates", len(matches))
	return id
}

// NewUplinkSink creates a sink; store and audit may be nil.
func NewUplinkSink(store UplinkStore, publish UplinkPublisher, auditSvc *audit.Service, actor string) *UplinkSink {
	return &UplinkSink{store: store, publish: publish, audit: auditSvc, actor: actor, now: time.Now}
}

// Handle processes raw uplink bytes from origin (IMEI or phone number) that
// arrived over bearer ("sms", "sbd", "imt", "globalstar") for the tenant.
// Returns false when the bytes are not an uplink frame.
func (u *UplinkSink) Handle(ctx context.Context, tenantID, bearer, origin string, raw []byte) bool {
	if !IsBridgeSatUplink(raw) {
		return false
	}
	msgType, payload, err := DecodeSatUplink(raw)
	if err != nil {
		slog.Warn("uplink: decode failed", "error", err, "bearer", bearer, "origin", origin)
		return false
	}
	bridgeID := ""
	switch msgType {
	case SatMsgPosition:
		id, lat, lon, alt, _, ts, err := DecodeSatPosition(payload)
		if err != nil {
			slog.Warn("uplink: position decode failed", "error", err, "bearer", bearer, "origin", origin)
			return true
		}
		bridgeID = u.resolveBridgeID(ctx, tenantID, id, bearer)
		slog.Info("uplink: bridge position", "bridge_id", bridgeID, "bearer", bearer, "lat", lat, "lon", lon, "alt", alt)
		u.pub(hubmqtt.TopicPositionFor(tenantID, bridgeID), 1, true, map[string]any{
			"lat": lat, "lon": lon, "alt": alt,
			"source": bearer + "_uplink", "bearer": bearer, "timestamp": ts.UTC().Format(time.RFC3339),
		})
		u.report(ctx, tenantID, bridgeID, bearer, ts)

	case SatMsgSOS:
		id, deviceID, lat, lon, message, ts, err := DecodeSatSOS(payload)
		if err != nil {
			slog.Warn("uplink: SOS decode failed", "error", err, "bearer", bearer, "origin", origin)
			return true
		}
		bridgeID = u.resolveBridgeID(ctx, tenantID, id, bearer)
		slog.Warn("uplink: BRIDGE SOS", "bridge_id", bridgeID, "device_id", deviceID, "bearer", bearer, "lat", lat, "lon", lon, "message", message)
		u.pub(hubmqtt.TopicSOSFor(tenantID, bridgeID), 1, false, map[string]any{
			"bridge_id": bridgeID, "device_id": deviceID, "lat": lat, "lon": lon, "message": message,
			"source": bearer + "_uplink", "bearer": bearer, "timestamp": ts.UTC().Format(time.RFC3339),
		})
		// The SOS detector listens on mo/decoded and raises the escalation
		// alert on sos=true; the id makes the claim stable across replicas.
		subject := deviceID
		if subject == "" {
			subject = bridgeID
		}
		text := message
		if text == "" {
			text = fmt.Sprintf("SOS from bridge %s via %s", bridgeID, bearer)
		}
		u.pub(hubmqtt.TopicMODecodedFor(tenantID, subject), 1, false, map[string]any{
			"id":        fmt.Sprintf("sos-%s-%s-%d", bearer, bridgeID, ts.Unix()),
			"imei":      subject,
			"bridge_id": bridgeID,
			"text":      text,
			"sos":       true,
			"channel":   bearer,
			"source":    "bridge_sos",
			"lat":       lat,
			"lon":       lon,
			"timestamp": ts.UTC().Format(time.RFC3339),
		})
		u.report(ctx, tenantID, bridgeID, bearer, ts)

	case SatMsgHealthSummary:
		id, uptimeSec, cpuPct, memPct, diskPct, ifaces, ts, err := DecodeSatHealth(payload)
		if err != nil {
			slog.Warn("uplink: health decode failed", "error", err, "bearer", bearer, "origin", origin)
			return true
		}
		bridgeID = u.resolveBridgeID(ctx, tenantID, id, bearer)
		slog.Info("uplink: bridge health", "bridge_id", bridgeID, "bearer", bearer, "uptime", uptimeSec, "cpu", cpuPct, "mem", memPct, "disk", diskPct, "interfaces", len(ifaces))
		if u.store != nil {
			list := make([]map[string]any, 0, len(ifaces))
			for _, i := range ifaces {
				status := "down"
				if i.Online {
					status = "up"
				}
				list = append(list, map[string]any{"name": i.Name, "status": status, "signal_bars": int(i.Signal)})
			}
			healthJSON, _ := json.Marshal(map[string]any{
				"bridge_id":  bridgeID,
				"uptime_sec": uptimeSec,
				"cpu_pct":    cpuPct,
				"mem_pct":    memPct,
				"disk_pct":   diskPct,
				"interfaces": list,
				"source":     bearer + "_uplink",
				"bearer":     bearer,
				"timestamp":  ts.UTC().Format(time.RFC3339),
			})
			if !u.ownsBridge(ctx, tenantID, bridgeID) {
				return true
			}
			if err := u.store.SetBridgeHealth(ctx, tenantID, bridgeID, string(healthJSON)); err != nil {
				slog.Warn("uplink: set health failed", "error", err, "bridge_id", bridgeID)
			}
		}
		u.report(ctx, tenantID, bridgeID, bearer, ts)

	default:
		slog.Warn("uplink: unknown frame type", "type", msgType, "bearer", bearer, "origin", origin)
	}

	if u.audit != nil {
		detail := fmt.Sprintf("origin=%s bearer=%s type=0x%02x bytes=%d bridge=%s", origin, bearer, msgType, len(raw), bridgeID)
		if err := u.audit.Log(ctx, tenantID, "bridge_uplink", u.actor, detail, ""); err != nil {
			slog.Warn("audit: bridge_uplink", "error", err)
		}
	}
	return true
}

func (u *UplinkSink) pub(topic string, qos byte, retained bool, v any) {
	if u.publish != nil {
		u.publish(topic, qos, retained, v)
	}
}

// report records last-seen and the bearer/time of the report. A frame's own
// timestamp is used when it is sane, else now.
func (u *UplinkSink) report(ctx context.Context, tenantID, bridgeID, bearer string, ts time.Time) {
	if u.store == nil || bridgeID == "" {
		return
	}
	now := u.now()
	if ts.IsZero() || ts.After(now.Add(5*time.Minute)) || ts.Before(now.Add(-30*24*time.Hour)) {
		ts = now
	}
	if !u.ownsBridge(ctx, tenantID, bridgeID) {
		return
	}
	if err := u.store.TouchBridgeLastSeen(ctx, tenantID, bridgeID); err != nil {
		slog.Debug("uplink: touch last_seen failed", "error", err, "bridge_id", bridgeID)
	}
	if err := u.store.SetBridgeLastReport(ctx, tenantID, bridgeID, bearer, ts); err != nil {
		slog.Debug("uplink: set last report failed", "error", err, "bridge_id", bridgeID)
	}
}
