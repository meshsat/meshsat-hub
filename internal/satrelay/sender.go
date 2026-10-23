// Package satrelay sends a satellite_relay payload to its recipient modem
// through the provider that modem is on: Cloudloop for a RockBLOCK 9704 (IMT),
// Rock7 for a RockBLOCK 9602/9603 (SBD). The payload goes out byte for byte;
// it is never cut, since no Bridge or phone reassembles the Hub's fragment
// header, so an SBD recipient takes at most one 270-byte MT frame.
// [MESHSAT-1623]
package satrelay

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/meshsat/meshsat-hub/internal/fragment"
	"github.com/meshsat/meshsat-hub/internal/rock7"
)

// SBDMTMax is the largest MT a 9602/9603 takes in one frame.
const SBDMTMax = fragment.IridiumMT_MTU

// sbdTypes are the device types the Hub registers for a Rock7 (SBD) modem.
var sbdTypes = map[string]bool{"rockblock": true, "iridium_sbd": true}

// MTSender sends one MT through Rock7 (rock7.Client).
type MTSender interface {
	SendMT(ctx context.Context, imei, dataHex string) (*rock7.SendResult, error)
}

// Sender picks the provider for each recipient. Every func except Record and
// Allow is required.
type Sender struct {
	// IsIMT reports whether the modem is a known Cloudloop IMT device.
	IsIMT func(tenantID, imei string) bool
	// SendIMT sends the wire to an IMT modem on the topic (empty = RAW).
	SendIMT func(imei, wireB64, imtTopic string) error
	// DeviceType returns the tenant's registered type for the modem;
	// sql.ErrNoRows when the tenant has no such device.
	DeviceType func(ctx context.Context, tenantID, imei string) (string, error)
	// Rock7 returns the tenant's Rock7 client, nil when it has none.
	Rock7 func(ctx context.Context, tenantID string) MTSender
	// Allow is the per-device rate limit the Cloudloop path applies too.
	Allow func(tenantID, imei string) bool
	// Record persists an SBD relay MT and publishes its status. mtID is
	// Rock7's id when it took the message; err is why it didn't.
	Record func(ctx context.Context, tenantID, imei string, wire []byte, mtID string, err error)
}

// Send relays wireB64 to imei. An IMT modem gets it on imtTopic; an SBD
// modem gets it through Rock7, which has no topic.
func (s *Sender) Send(ctx context.Context, tenantID, imei, wireB64, imtTopic string) error {
	if s.IsIMT(tenantID, imei) {
		return s.SendIMT(imei, wireB64, imtTopic)
	}
	typ, err := s.DeviceType(ctx, tenantID, imei)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("modem %s is neither a known IMT device nor a registered Rock7 (SBD) device of this tenant", imei)
	}
	if err != nil {
		return fmt.Errorf("modem %s: device lookup failed: %w", imei, err)
	}
	if !sbdTypes[typ] {
		return fmt.Errorf("modem %s is a %q device, which the relay cannot reach", imei, typ)
	}
	return s.sendSBD(ctx, tenantID, imei, wireB64)
}

func (s *Sender) sendSBD(ctx context.Context, tenantID, imei, wireB64 string) error {
	wire, err := base64.StdEncoding.DecodeString(wireB64)
	if err != nil || len(wire) == 0 {
		return fmt.Errorf("wire is not a base64 payload")
	}
	fail := func(err error) error {
		if s.Record != nil {
			s.Record(ctx, tenantID, imei, wire, "", err)
		}
		return err
	}
	if len(wire) > SBDMTMax {
		return fail(fmt.Errorf("verbatim payload is %d bytes, over the %d byte MT frame of an SBD modem", len(wire), SBDMTMax))
	}
	if s.Allow != nil && !s.Allow(tenantID, imei) {
		return fail(fmt.Errorf("device rate limit exceeded"))
	}
	c := s.Rock7(ctx, tenantID)
	if c == nil {
		return fail(fmt.Errorf("no Rock7 account configured for tenant %s", tenantID))
	}
	res, err := c.SendMT(ctx, imei, hex.EncodeToString(wire))
	if err != nil {
		return fail(err)
	}
	if s.Record != nil {
		s.Record(ctx, tenantID, imei, wire, res.MTID, nil)
	}
	return nil
}
