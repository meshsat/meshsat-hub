// Package bearers delivers OOB frames over the Hub's outbound legs: Twilio
// SMS to the kit's SIM, Cloudloop IMT to a 9704 modem, Rock7 MT to a 9603
// modem (MESHSAT-964 C). Each transport uses the tenant's own provider
// account through the client pools.
package bearers

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/cloudloop"
	"github.com/meshsat/meshsat-hub/internal/rock7"
	"github.com/meshsat/meshsat-hub/internal/sms"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// SMS sends the frame text as an SMS to the peer's phone number.
type SMS struct {
	Pool *sms.ClientPool
	Wait time.Duration
}

// Send implements oob.Transport.
func (t *SMS) Send(ctx context.Context, tenantID string, peer *store.OOBPeer, text string) error {
	if peer.Phone == "" {
		return errors.New("bridge has no phone number")
	}
	client := t.Pool.ForTenant(ctx, tenantID)
	if client == nil {
		return errors.New("no Twilio account configured for this tenant")
	}
	_, err := client.Send(ctx, peer.Phone, text)
	return err
}

// Timeout implements oob.Transport.
func (t *SMS) Timeout() time.Duration { return t.Wait }

// IMT sends the frame text as a Cloudloop IMT MT payload to the peer's modem.
type IMT struct {
	Pool     *cloudloop.ClientPool
	Resolver *cloudloop.ThingResolver
	Wait     time.Duration
}

// Send implements oob.Transport.
func (t *IMT) Send(ctx context.Context, tenantID string, peer *store.OOBPeer, text string) error {
	if peer.SatIMEI == "" {
		return errors.New("bridge has no satellite IMEI")
	}
	client := t.Pool.ForTenant(ctx, tenantID)
	if client == nil {
		return errors.New("no Cloudloop account configured for this tenant")
	}
	thing := peer.SatIMEI
	if t.Resolver != nil {
		thing, _ = t.Resolver.Resolve(tenantID, peer.SatIMEI)
	}
	_, err := client.SendIMT(ctx, thing, []byte(text), "", "")
	return err
}

// Timeout implements oob.Transport.
func (t *IMT) Timeout() time.Duration { return t.Wait }

// SBD sends the frame text as a Rock7 MT payload to the peer's 9603 modem.
type SBD struct {
	Pool *rock7.ClientPool
	Wait time.Duration
}

// Send implements oob.Transport.
func (t *SBD) Send(ctx context.Context, tenantID string, peer *store.OOBPeer, text string) error {
	if peer.SatIMEI == "" {
		return errors.New("bridge has no satellite IMEI")
	}
	client := t.Pool.ForTenant(ctx, tenantID)
	if client == nil {
		return errors.New("no Rock7 account configured for this tenant")
	}
	_, err := client.SendMT(ctx, peer.SatIMEI, hex.EncodeToString([]byte(text)))
	return err
}

// Timeout implements oob.Transport.
func (t *SBD) Timeout() time.Duration { return t.Wait }
