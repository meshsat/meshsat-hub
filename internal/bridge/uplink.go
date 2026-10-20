package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"log/slog"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/hubmsg"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/sos"
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
	runs    SOSRuns           // C10: starts and cancels that carry their identity; nil = old path only
}

// SOSRuns applies an SOS start or cancel that carries its identity (HeMB-NG C10).
type SOSRuns interface {
	Apply(ctx context.Context, ev sos.Event) (store.SOSRunApplied, error)
	// PageUnstored pages a valid start the database did not take, under its run's id.
	PageUnstored(ctx context.Context, ev sos.Event) bool
}

// SetSOSRuns lets the sink take the C10 frames 0x07 and 0x08. Without it they are logged,
// and a start still pages the old way.
func (u *UplinkSink) SetSOSRuns(r SOSRuns) *UplinkSink { u.runs = r; return u }

// deviceBridger is the optional part of the store that names a modem's bridge.
type deviceBridger interface {
	DeviceBridgeID(ctx context.Context, tenantID string, imei string) (string, error)
}

// authBridge is the bridge the ingress vouches for, for a C10 frame naming frameBridge
// (sos.json's rule): over a satellite modem, the bridge the modem belongs to, which must be
// the one the frame names; over SMS, which links no number to a bridge, the tenant's bridge
// the frame names (the tenant comes from the webhook path, the sender's number from Twilio).
// why is non-empty when the frame cannot be attributed.
func (u *UplinkSink) authBridge(ctx context.Context, tenantID, bearer, origin, frameBridge string) (bridgeID, why string) {
	if frameBridge == "" {
		return "", "the frame names no bridge"
	}
	if bearer == "sms" {
		if !u.ownsBridge(ctx, tenantID, frameBridge) {
			return "", "the bridge belongs to another tenant"
		}
		return frameBridge, ""
	}
	db, ok := u.store.(deviceBridger)
	if !ok {
		return "", "no modem-to-bridge link in this store"
	}
	owner, err := db.DeviceBridgeID(ctx, tenantID, origin)
	switch {
	case err != nil:
		return "", "modem lookup failed: " + err.Error()
	case owner == "":
		return "", "the modem belongs to no bridge"
	case owner != frameBridge:
		return "", "the frame names " + frameBridge + ", the modem belongs to " + owner
	}
	return owner, ""
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

// bridgeGetter is the optional part of the store that lets the sink see how
// the bridge last reported.
type bridgeGetter interface {
	GetBridge(ctx context.Context, tenantID, bridgeID string) (*store.Bridge, error)
}

// mqttFreshFor is how recent an MQTT report must be for the bridge to count as
// live over MQTT. Kits send MQTT health every minute or two.
const mqttFreshFor = 10 * time.Minute

// liveOverMQTT reports whether the bridge is online and reported over MQTT
// within mqttFreshFor. A satellite or SMS frame from such a bridge is not its
// report of record. Since 21 Sep 2026 a quiet kit sends a health frame over
// IMT every ten minutes only to open a session so waiting MTs come down, with
// MQTT up the whole time. Letting that frame win would show the kit on the
// Fleet page as reporting over satellite while it is on MQTT, and replace
// its full MQTT health with the frame's summary. When MQTT is really gone, the
// last MQTT report ages past mqttFreshFor, and the frame becomes the report,
// as before. A failed lookup answers "not live", so a frame is never dropped
// on a database blip.
func (u *UplinkSink) liveOverMQTT(ctx context.Context, tenantID, bridgeID string) bool {
	g, ok := u.store.(bridgeGetter)
	if !ok || bridgeID == "" {
		return false
	}
	b, err := g.GetBridge(ctx, tenantID, bridgeID)
	if err != nil || b == nil || !b.Online || b.LastReportBearer != "mqtt" || b.LastReportAt == nil {
		return false
	}
	return u.now().Sub(*b.LastReportAt) < mqttFreshFor
}

// bridgeSelfDevice is the device id a bridge puts in an SOS it raised itself,
// rather than one relayed from a node on its mesh.
const bridgeSelfDevice = "bridge"

// resolveBridgeID turns the id inside a frame into a bridge this tenant has.
//
// The field kits' encoder cut the id at 16 bytes, so an 18-character id
// arrived two characters short (2026-09-21). The health update then ran against
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
		u.legacySOS(tenantID, bridgeID, bearer, deviceID, message, lat, lon, ts)
		u.report(ctx, tenantID, bridgeID, bearer, ts)

	case SatMsgSOSStart:
		f, err := DecodeSatSOSStart(payload)
		if err != nil {
			slog.Warn("uplink: SOS start (0x07) decode failed", "error", err, "bearer", bearer, "origin", origin)
			return true
		}
		bridgeID = u.resolveBridgeID(ctx, tenantID, f.BridgeID, bearer)
		sosID := hubmsg.UUIDText(f.SOSID)
		auth, why := u.authBridge(ctx, tenantID, bearer, origin, bridgeID)
		if why == "" && u.runs == nil {
			why = "this ingress does not take C10 runs"
		}
		if why == "" {
			ev := sos.Event{
				TenantID: tenantID, BridgeID: auth, Bearer: bearer, Kind: store.SOSEventStart, SOSID: sosID,
				RunStartMs: f.RunStartMs, DeviceID: f.DeviceID, Text: f.Message, Lat: f.Lat, Lon: f.Lon,
				HasPosition: f.Lat != 0 || f.Lon != 0,
			}
			_, err := u.runs.Apply(ctx, ev)
			switch {
			case err == nil:
			case errors.Is(err, sos.ErrBadSOSID):
				why = err.Error()
			default:
				// Valid, but the database did not take the run (after retries). This bearer
				// sends the frame once, so it pages now, under the run's own alert id: a copy
				// that stores the run later finds its alert raised (r16 review C10-R2).
				slog.Error("uplink: SOS start not stored; paging under its run's id", "error", err, "sos_id", sosID,
					"bridge_id", bridgeID, "bearer", bearer)
				if !u.runs.PageUnstored(ctx, ev) {
					why = "not stored, and not paged under its run's id: " + err.Error()
				}
			}
		}
		if why != "" {
			// Never drop an SOS: one that cannot be taken as its run pages the old way, and
			// changes no run, so it can neither cancel nor be mistaken for one.
			slog.Warn("uplink: SOS start not taken as its run; paging the old way", "why", why, "sos_id", sosID,
				"bridge_id", bridgeID, "bearer", bearer, "origin", origin)
			u.legacySOS(tenantID, bridgeID, bearer, f.DeviceID, f.Message, f.Lat, f.Lon, f.Timestamp)
		}
		u.report(ctx, tenantID, bridgeID, bearer, f.Timestamp)

	case SatMsgSOSCancel:
		f, err := DecodeSatSOSCancel(payload)
		if err != nil {
			slog.Warn("uplink: SOS cancel (0x08) decode failed", "error", err, "bearer", bearer, "origin", origin)
			return true
		}
		bridgeID = u.resolveBridgeID(ctx, tenantID, f.BridgeID, bearer)
		sosID := hubmsg.UUIDText(f.SOSID)
		auth, why := u.authBridge(ctx, tenantID, bearer, origin, bridgeID)
		if why == "" && bearer == "sms" && !hubmsg.ValidV7(sosID) {
			// An old-format id is UUIDv5 of the bridge id and the run's start, which an SMS
			// sender could work out; only a v7 id, random past its time, may cancel by SMS.
			why = "an old-format SOS id cannot be cancelled over SMS"
		}
		if why == "" && u.runs == nil {
			why = "this ingress does not take C10 runs"
		}
		if why == "" {
			if _, err := u.runs.Apply(ctx, sos.Event{
				TenantID: tenantID, BridgeID: auth, Bearer: bearer, Kind: store.SOSEventCancel, SOSID: sosID,
				RunStartMs: f.RunStartMs, Reason: int(f.Reason),
			}); err != nil {
				why = err.Error()
			}
		}
		if why != "" {
			slog.Warn("uplink: SOS cancel refused; nothing changed", "why", why, "sos_id", sosID, "bridge_id", bridgeID,
				"bearer", bearer, "origin", origin)
		}
		u.report(ctx, tenantID, bridgeID, bearer, f.Timestamp)

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
			if u.liveOverMQTT(ctx, tenantID, bridgeID) {
				slog.Debug("uplink: health frame from a bridge live over MQTT; its MQTT health stays the report", "bridge_id", bridgeID, "bearer", bearer)
			} else if err := u.store.SetBridgeHealth(ctx, tenantID, bridgeID, string(healthJSON)); err != nil {
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

// legacySOS raises an SOS the old way (MESHSAT-964 B): the SOS topic for the Hub's
// consumers, and mo/decoded with sos=true for the Detector, which pages once per bridge and
// start second whichever bearer carried it.
func (u *UplinkSink) legacySOS(tenantID, bridgeID, bearer, deviceID, message string, lat, lon float64, ts time.Time) {
	slog.Warn("uplink: BRIDGE SOS", "bridge_id", bridgeID, "device_id", deviceID, "bearer", bearer, "lat", lat, "lon", lon, "message", message)
	u.pub(hubmqtt.TopicSOSFor(tenantID, bridgeID), 1, false, map[string]any{
		"bridge_id": bridgeID, "device_id": deviceID, "lat": lat, "lon": lon, "message": message,
		"source": bearer + "_uplink", "bearer": bearer, "timestamp": ts.UTC().Format(time.RFC3339),
	})
	// The SOS detector listens on mo/decoded and raises the escalation
	// alert on sos=true; the id makes the claim stable across replicas.
	subject := deviceID
	if subject == "" || subject == bridgeSelfDevice {
		// The bridge raised it itself (its panel or API). The frame then
		// names the device "bridge", and an alert under that name paged the
		// on-call person with "[sos] bridge: ..." -- with two kits at a
		// stand, no way to tell which one (MESHSAT-1294). A mesh node's SOS
		// relayed by the bridge keeps the node's own id.
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
	if u.liveOverMQTT(ctx, tenantID, bridgeID) {
		return // its MQTT report stays the one on the Fleet page
	}
	if err := u.store.SetBridgeLastReport(ctx, tenantID, bridgeID, bearer, ts); err != nil {
		slog.Debug("uplink: set last report failed", "error", err, "bridge_id", bridgeID)
	}
}
