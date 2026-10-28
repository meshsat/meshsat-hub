package takchat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// How a dispatcher's chat reaches a kit (MESHSAT-1744). MQTT while the kit
// is on it; else, if the workspace allows it, a satellite MT to the modem
// the kit's birth names (IMT on the tenant's Cloudloop account, SBD on its
// Rock7 one: dollars); else SMS to the kit's own SIM from the Hub's number
// (cents); else nothing, and the operator is told so.

// The bearers a message is recorded under.
const (
	BearerMQTT = "mqtt"
	BearerSBD  = "sbd"
	BearerIMT  = "imt"
	BearerSMS  = "sms"
	// BearerSatellite is the word a tenant may force; it resolves to sbd
	// or imt by the modem.
	BearerSatellite = "satellite"
)

// ForceBearers are the words crew_chat_force_bearer takes.
var ForceBearers = map[string]bool{"": true, BearerMQTT: true, BearerSatellite: true, BearerSMS: true}

// ErrNoBearer: the kit is off MQTT and nothing else reaches it.
var ErrNoBearer = errors.New("takchat: the kit is off MQTT and no other bearer reaches it")

// Bearers are the Hub's ways to a kit that is off MQTT. Each may be nil,
// which turns that bearer off.
type Bearers struct {
	// SendMT sends one frame to a modem as a satellite MT: IMT through the
	// tenant's Cloudloop account, SBD through its Rock7 one, the bytes
	// verbatim.
	SendMT func(ctx context.Context, tenantID, imei string, wire []byte) error
	// IsIMT says whether a modem takes IMT (a 9704) rather than SBD.
	IsIMT func(tenantID, imei string) bool
	// SendSMS sends one SMS to a kit's own SIM from the Hub's number (the
	// booth's smsKitSender: the kit takes the Hub's number as a plaintext
	// peer and answers to it).
	SendSMS func(ctx context.Context, tenantID, bridgeID, body string) error
	// OfflineTimeout is how long after its last report a bridge still
	// counts as on MQTT: the tenant's own choice within the platform's
	// bounds, else the platform default. nil = five minutes.
	OfflineTimeout func(t *store.Tenant) time.Duration
}

// SetBearers gives the service its ways to a kit off MQTT. Call it before
// any Send.
func (s *Service) SetBearers(b Bearers) { s.bearers = b }

// Settings are a workspace's crew chat choices (store.Tenant).
type Settings struct {
	Satellite   bool   // satellite fallback on (spend); off by default
	SMS         bool   // SMS fallback on; on by default
	ForceBearer string // "", mqtt, satellite or sms: a test's choice
}

// SettingsOf reads a tenant's choices; nil reads as the defaults.
func SettingsOf(t *store.Tenant) Settings {
	if t == nil {
		return Settings{SMS: true}
	}
	return Settings{Satellite: t.CrewChatSatellite, SMS: !t.CrewChatSMSOff, ForceBearer: strings.ToLower(strings.TrimSpace(t.CrewChatForceBearer))}
}

// choice is the bearer picked for one message.
type choice struct {
	Bearer string // mqtt, sbd, imt or sms
	IMEI   string // the modem, for sbd and imt
	Forced bool   // a test's choice, not the kit's state
	Why    string // why the frame bearers were not taken, for the error
}

// defaultOfflineTimeout is the platform's default, as config.go sets it.
const defaultOfflineTimeout = 5 * time.Minute

// chooseBearer picks how the message to crew on bridge b goes.
func (s *Service) chooseBearer(ctx context.Context, t *store.Tenant, b *store.Bridge, now time.Time) (choice, error) {
	cfg := SettingsOf(t)
	timeout := defaultOfflineTimeout
	if s.bearers.OfflineTimeout != nil {
		if d := s.bearers.OfflineTimeout(t); d > 0 {
			timeout = d
		}
	}
	onMQTT := b.Online && b.LastSeen != nil && now.Sub(*b.LastSeen) <= timeout &&
		(b.LastReportBearer == "" || b.LastReportBearer == BearerMQTT)
	switch cfg.ForceBearer {
	case "":
		if onMQTT {
			return choice{Bearer: BearerMQTT}, nil
		}
	case BearerMQTT:
		return choice{Bearer: BearerMQTT, Forced: true}, nil
	case BearerSatellite:
		c, why := s.satelliteChoice(ctx, t, b)
		if why != "" {
			return choice{}, fmt.Errorf("%w (satellite forced: %s)", ErrNoBearer, why)
		}
		c.Forced = true
		return c, nil
	case BearerSMS:
		c, why := s.smsChoice(ctx, t, b)
		if why != "" {
			return choice{}, fmt.Errorf("%w (SMS forced: %s)", ErrNoBearer, why)
		}
		c.Forced = true
		return c, nil
	default:
		return choice{}, fmt.Errorf("%w (crew_chat_force_bearer %q is not a bearer)", ErrNoBearer, cfg.ForceBearer)
	}
	var whys []string
	if cfg.Satellite {
		c, why := s.satelliteChoice(ctx, t, b)
		if why == "" {
			return c, nil
		}
		whys = append(whys, "satellite: "+why)
	} else {
		whys = append(whys, "satellite: off for this workspace")
	}
	if cfg.SMS {
		c, why := s.smsChoice(ctx, t, b)
		if why == "" {
			return c, nil
		}
		whys = append(whys, "SMS: "+why)
	} else {
		whys = append(whys, "SMS: off for this workspace")
	}
	return choice{}, fmt.Errorf("%w (%s)", ErrNoBearer, strings.Join(whys, "; "))
}

// satelliteChoice names the modem a frame to the bridge goes to, or why
// none does.
func (s *Service) satelliteChoice(ctx context.Context, t *store.Tenant, b *store.Bridge) (choice, string) {
	if s.bearers.SendMT == nil {
		return choice{}, "no satellite sender on this Hub"
	}
	imei := s.modemOf(ctx, b)
	if imei == "" {
		return choice{}, "the kit's birth names no satellite modem"
	}
	if dev, err := s.store.GetDevice(ctx, b.TenantID, imei); err != nil || dev == nil {
		return choice{}, "modem " + imei + " is not registered to this workspace"
	}
	bearer := BearerSBD
	if s.bearers.IsIMT != nil && s.bearers.IsIMT(b.TenantID, imei) {
		bearer = BearerIMT
	}
	return choice{Bearer: bearer, IMEI: imei}, ""
}

// modemOf is the satellite modem a bridge's last birth names: an online
// iridium interface first, else any with an IMEI; else the OOB pairing's.
func (s *Service) modemOf(ctx context.Context, b *store.Bridge) string {
	var birth protocol.BridgeBirth
	if b.LastBirth != "" {
		_ = json.Unmarshal([]byte(b.LastBirth), &birth)
	}
	any := ""
	for _, i := range birth.Interfaces {
		if i.IMEI == "" || !strings.HasPrefix(i.Type, "iridium") {
			continue
		}
		if i.Status == "online" {
			return i.IMEI
		}
		if any == "" {
			any = i.IMEI
		}
	}
	if any != "" {
		return any
	}
	if peer, err := s.store.GetOOBPeer(ctx, b.TenantID, b.BridgeID); err == nil && peer != nil {
		return peer.SatIMEI
	}
	return ""
}

// smsChoice says whether an SMS to the kit's own SIM can go, or why not.
func (s *Service) smsChoice(ctx context.Context, t *store.Tenant, b *store.Bridge) (choice, string) {
	if s.bearers.SendSMS == nil {
		return choice{}, "no SMS sender on this Hub"
	}
	var birth protocol.BridgeBirth
	if b.LastBirth != "" {
		_ = json.Unmarshal([]byte(b.LastBirth), &birth)
	}
	if len(birth.Interfaces) > 0 {
		cellular := false
		for _, i := range birth.Interfaces {
			if i.Type == "cellular" {
				cellular = true
			}
		}
		if !cellular {
			return choice{}, "the kit's birth names no cellular interface"
		}
	}
	peer, err := s.store.GetOOBPeer(ctx, b.TenantID, b.BridgeID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return choice{}, "the kit's pairing could not be read"
	}
	if peer == nil || strings.TrimSpace(peer.Phone) == "" {
		return choice{}, "the kit has no phone number on file (its out-of-band pairing)"
	}
	return choice{Bearer: BearerSMS}, ""
}
